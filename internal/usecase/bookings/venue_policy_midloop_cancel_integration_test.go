package bookings

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
)

// MID-LOOP CANCEL. The sibling file guards the mid-READ skip: the switch reads
// its booking set in ONE statement, so a cancel that commits while the read is
// in flight can no longer make a row disappear from the set.
//
// This file covers what that fix does NOT address and never claimed to. The set
// is a snapshot; the WRITES that follow it are not. Nothing serialises a status
// change against a mode switch:
//
//   - status.go never calls capacity.LockVenue (the advisory lock guards
//     CREATES), so a guest tapping "cancel" runs freely alongside the switch;
//   - sqltx.Manager begins with no TxOptions, i.e. READ COMMITTED, so each
//     statement of the switch sees whatever has committed by the time it runs.
//
// So a booking that was live when the set was read can be cancelled while the
// switch is still walking that set. The question these tests answer — against a
// real database, because only the database can answer it — is what the cancelled
// booking ends up holding.
//
// The safety net is meant to be migrations 0057 / 0058: a BEFORE INSERT trigger
// derives `active` from the booking's CURRENT committed status, so a hold or a
// link written for an already-cancelled booking lands INACTIVE and takes
// nothing. Both directions of the switch are checked here, and the assertion is
// the invariant itself: a cancelled booking must occupy nothing.
//
// THE INVARIANT, stated so it cannot be "fixed" by relaxing an expectation:
// after a mode switch, a booking whose cancel is committed at ANY point before
// the switch commits must hold NO ACTIVE capacity hold and NO ACTIVE table
// link. An active row for a cancelled guest is phantom occupancy — a seat or a
// table the venue can never sell and never serve.

// skipOpenHole marks the two tests that REPRODUCE a hole which is still open.
// They are skipped, not deleted and not weakened: their assertions are the
// invariant the product must hold, and they were verified to FAIL against this
// commit — that failure is the bug report, not a wrong expectation.
//
// OBSERVED, 25.07.2026, against the migrated test database at version 58:
//
//	tables → seats, cancel committed after the switch INSERTed the holds:
//	  10 rows in booking_capacity_holds for the cancelled booking, ALL active,
//	  restaurant_capacity_buckets reporting 4 seats taken across its whole
//	  window. Still 10 active after a further capacity change — nothing in the
//	  product walks cancelled bookings, so it never heals.
//	seats → tables, cancel committed after the switch INSERTed the placement:
//	  1 active row in booking_tables for the cancelled booking, and seating
//	  another party of 4 at that table in that slot is refused by the GiST
//	  exclusion constraint ("already exists"). The table is lost for the slot.
//
// Why the trigger does not save this half: migrations 0057 / 0058 derive
// `active` at INSERT time from the status committed AT THAT MOMENT, which was
// still live; and the status trigger that would flip the row afterwards
// (0004 / 0054, `UPDATE … WHERE booking_id = NEW.id`) cannot see a row the
// switch's transaction has not committed yet. Neither side is wrong on its own —
// they simply do not compose under READ COMMITTED with no lock shared between a
// status change and a mode switch.
//
// REMOVE THIS SKIP as part of the fix. Do not adjust the assertions.
const skipOpenHole = "reproduces an OPEN hole (mid-loop cancel leaves phantom occupancy) — see the comment on skipOpenHole; unskip as part of the fix, do not relax the assertions"

// hookedCapacity decorates the real capacity repository so a test can commit
// something out of band immediately before or immediately after one chosen
// booking's holds are written. The hook lives here, in the test, exactly like
// cancellingLister in the sibling file — production code is not made testable,
// it is called ordinarily.
type hookedCapacity struct {
	domain.BookingCapacityRepository
	before, after func(bookingID uuid.UUID)
}

func (c *hookedCapacity) ReplaceForBooking(
	ctx context.Context, bookingID uuid.UUID, holds []domain.BookingCapacityHold,
) error {
	if c.before != nil {
		c.before(bookingID)
	}
	err := c.BookingCapacityRepository.ReplaceForBooking(ctx, bookingID, holds)
	if c.after != nil {
		c.after(bookingID)
	}
	return err
}

// hookedTables is the same idea on the seats → tables side, where the write
// under test is the placement insert.
type hookedTables struct {
	domain.BookingTableRepository
	before, after func(bookingID uuid.UUID)
}

func (l *hookedTables) Create(ctx context.Context, links []domain.BookingTable) error {
	var id uuid.UUID
	if len(links) > 0 {
		id = links[0].BookingID
	}
	if l.before != nil {
		l.before(id)
	}
	err := l.BookingTableRepository.Create(ctx, links)
	if l.after != nil {
		l.after(id)
	}
	return err
}

// midLoopHarness is the guarantee harness with both write ports decorated and a
// policy usecase built over the decorated ones.
type midLoopHarness struct {
	*guaranteeHarness
	capHook   *hookedCapacity
	linkHook  *hookedTables
	midPolicy PolicyUseCase
}

func newMidLoopHarness(t *testing.T, mode string, seats int) *midLoopHarness {
	t.Helper()
	h := newGuaranteeHarness(t, mode, seats)
	capHook := &hookedCapacity{BookingCapacityRepository: h.capacity}
	linkHook := &hookedTables{BookingTableRepository: bookingrepo.NewTables(h.pool)}
	return &midLoopHarness{
		guaranteeHarness: h,
		capHook:          capHook,
		linkHook:         linkHook,
		midPolicy: NewPolicyUseCase(h.restRepo, h.restRepo,
			newFakeManagers([2]uuid.UUID{h.manager.UserID, h.restaurantID}),
			h.related, capHook, linkHook, h.bookings, h.txm, testConfig()),
	}
}

// cancelOutOfBand commits a cancellation on its own pool connection — a separate
// backend, its own transaction, committed the moment Exec returns. That is a
// guest tapping "cancel" in the app while the owner holds the venue lock.
//
// The context has a deadline so that an interleaving which BLOCKS (a lock the
// switch holds) is reported as such instead of hanging the suite: the switch
// cannot commit while this hook has not returned, so a block here would be a
// deadlock.
func (h *midLoopHarness) cancelOutOfBand(t *testing.T, bookingID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx,
		`UPDATE bookings SET status='cancelled', cancelled_at=now(), updated_at=now() WHERE id=$1`,
		bookingID); err != nil {
		t.Fatalf("out-of-band cancel of %s: %v", bookingID, err)
	}
}

// activeHolds / activeLinks answer the only question that matters: does the
// cancelled booking still occupy anything?
func (h *midLoopHarness) activeHolds(t *testing.T, bookingID uuid.UUID) []domain.BookingCapacityHold {
	t.Helper()
	var out []domain.BookingCapacityHold
	for _, hold := range h.holds(t, bookingID) {
		if hold.Active {
			out = append(out, hold)
		}
	}
	return out
}

func (h *midLoopHarness) activeLinks(t *testing.T, bookingID uuid.UUID) []domain.BookingTable {
	t.Helper()
	var out []domain.BookingTable
	for _, l := range h.links(t, bookingID) {
		if l.Active {
			out = append(out, l)
		}
	}
	return out
}

// takenIn is the WORST bucket counter over one booking's occupancy window — the
// same "minimum free seats across the window" rule freeSeats applies, read from
// the other side. The counters are what the seats engine actually sells against,
// so they are the consequence the `active` flag has: a phantom active hold shows
// up here as seats nobody is taking.
func (h *midLoopHarness) takenIn(t *testing.T, start time.Time) int {
	t.Helper()
	from, to := occupancyWindow(start, domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute})
	var taken int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT coalesce(max(seats_taken),0) FROM restaurant_capacity_buckets
		 WHERE restaurant_id=$1 AND bucket_start >= $2 AND bucket_start < $3`,
		h.restaurantID, from, to).Scan(&taken); err != nil {
		t.Fatalf("read buckets: %v", err)
	}
	return taken
}

// midLoopSeed writes three confirmed bookings of four guests at 12:00, 16:00 and
// 20:00 local. The times are deliberately far enough apart that their occupancy
// windows (2h + 15m buffers) do not overlap, so each booking owns its own
// buckets and the counters can be read per booking. The middle one is the
// victim: liveBookings reads `ORDER BY starts_at, id`, so the switch is still
// walking the set — and still writing — when it is cancelled.
func (h *midLoopHarness) midLoopSeed(t *testing.T) (first, victim, last *domain.Booking) {
	t.Helper()
	first = h.seedBooking(t, h.slotAt(t, 12), 4, domain.BookingConfirmed)
	victim = h.seedBooking(t, h.slotAt(t, 16), 4, domain.BookingConfirmed)
	last = h.seedBooking(t, h.slotAt(t, 20), 4, domain.BookingConfirmed)
	return first, victim, last
}

// TestTablesToSeatsSwitchGivesNoActiveHoldToABookingCancelledBeforeItsWrite is
// the interleaving the 0057 trigger is built for: the cancel is committed while
// the switch is inside the loop and has NOT yet written this booking's holds.
// The BEFORE INSERT trigger reads the booking's status under READ COMMITTED,
// sees `cancelled`, and lands the holds inactive.
func TestTablesToSeatsSwitchGivesNoActiveHoldToABookingCancelledBeforeItsWrite(t *testing.T) {
	h := newMidLoopHarness(t, "tables", 0)
	ctx := context.Background()
	first, victim, last := h.midLoopSeed(t)

	h.capHook.before = func(id uuid.UUID) {
		if id == victim.ID {
			h.cancelOutOfBand(t, victim.ID)
		}
	}

	if _, err := h.midPolicy.Update(ctx, h.manager, h.restaurantID, seatsOverride(100)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	// The premise: the cancel really did commit, and the switch really did write
	// rows for this booking. A test where either half did not happen proves
	// nothing.
	if got := h.status(t, victim.ID); got != domain.BookingCancelled {
		t.Fatalf("victim status = %q, want cancelled: the out-of-band cancel did not take effect", got)
	}
	if len(h.holds(t, victim.ID)) == 0 {
		t.Fatalf("the switch wrote no hold rows at all for the cancelled booking; this test's premise is gone")
	}

	// THE INVARIANT. A cancelled guest occupies nothing.
	if act := h.activeHolds(t, victim.ID); len(act) != 0 {
		t.Errorf("INVARIANT BROKEN: a booking cancelled mid-switch came out holding %d ACTIVE capacity holds (%+v). Those seats are phantom occupancy: the venue can never sell them and no guest will ever take them. This is what the BEFORE INSERT trigger of migration 0057 exists to prevent — if you are here because you changed that trigger, or made the switch write `active` from Go, you have reopened the hole, not fixed a test.",
			len(act), act)
	}
	if taken := h.takenIn(t, victim.StartsAt); taken != 0 {
		t.Errorf("INVARIANT BROKEN: the buckets of the cancelled booking's window report %d seats taken, want 0 (phantom occupancy)", taken)
	}

	// And the switch did its job for everybody who is still live: the mid-loop
	// cancel must not cost the OTHER bookings their holds.
	for _, b := range []*domain.Booking{first, last} {
		if act := h.activeHolds(t, b.ID); len(act) == 0 {
			t.Errorf("live booking %s came out of the switch with no active holds: its seats will be sold twice", b.ID)
		}
		if taken := h.takenIn(t, b.StartsAt); taken != 4 {
			t.Errorf("live booking %s: seats_taken in its window = %d, want 4", b.ID, taken)
		}
	}
}

// TestTablesToSeatsSwitchGivesNoActiveHoldToABookingCancelledAfterItsWrite is
// the OTHER half of the same window, and it is the one no trigger obviously
// covers: the holds for this booking have already been INSERTed by the switch
// (uncommitted), and only then does the cancel commit. The status trigger of
// 0054 flips holds with `UPDATE booking_capacity_holds ... WHERE booking_id=…`,
// and an uncommitted row of another transaction is invisible to it — so there is
// nothing for it to flip, and 0057's derivation already ran against the status
// as it was BEFORE the cancel.
//
// The invariant is the same either way: a cancelled guest occupies nothing.
func TestTablesToSeatsSwitchGivesNoActiveHoldToABookingCancelledAfterItsWrite(t *testing.T) {
	t.Skip(skipOpenHole)
	h := newMidLoopHarness(t, "tables", 0)
	ctx := context.Background()
	first, victim, last := h.midLoopSeed(t)

	h.capHook.after = func(id uuid.UUID) {
		if id == victim.ID {
			h.cancelOutOfBand(t, victim.ID)
		}
	}

	if _, err := h.midPolicy.Update(ctx, h.manager, h.restaurantID, seatsOverride(100)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	if got := h.status(t, victim.ID); got != domain.BookingCancelled {
		t.Fatalf("victim status = %q, want cancelled: the out-of-band cancel did not take effect", got)
	}
	if len(h.holds(t, victim.ID)) == 0 {
		t.Fatalf("the switch wrote no hold rows at all for the cancelled booking; this test's premise is gone")
	}

	if act := h.activeHolds(t, victim.ID); len(act) != 0 {
		t.Errorf("INVARIANT BROKEN: a booking cancelled after its holds were written (but before the switch committed) came out holding %d ACTIVE capacity holds (%+v) — phantom occupancy for a guest who is not coming.",
			len(act), act)
	}
	if taken := h.takenIn(t, victim.StartsAt); taken != 0 {
		t.Errorf("INVARIANT BROKEN: the buckets of the cancelled booking's window report %d seats taken, want 0 (phantom occupancy)", taken)
	}

	for _, b := range []*domain.Booking{first, last} {
		if act := h.activeHolds(t, b.ID); len(act) == 0 {
			t.Errorf("live booking %s came out of the switch with no active holds: its seats will be sold twice", b.ID)
		}
	}

	// And nothing in the product ever cleans it up: every later reconciliation
	// walks LIVE bookings only, so a cancelled booking's holds are never
	// revisited. The phantom is permanent.
	if _, err := h.midPolicy.Update(ctx, h.manager, h.restaurantID, seatsOverride(120)); err != nil {
		t.Fatalf("second capacity change: %v", err)
	}
	if act := h.activeHolds(t, victim.ID); len(act) != 0 {
		t.Errorf("INVARIANT BROKEN, PERMANENTLY: the phantom holds survived a later capacity change (%d still active) — nothing walks cancelled bookings, so those seats stay sold to nobody until the rows are deleted by hand",
			len(act))
	}
}

// midLoopSeedSeats is the seats-mode premise: three table-less bookings that
// already hold seats in the ledger, and three tables to seat them at on the way
// back. One table per booking, sized exactly for it, so the placement is forced
// and a phantom link is immediately visible as a table nobody can use.
func (h *midLoopHarness) midLoopSeedSeats(t *testing.T) (first, victim, last *domain.Booking, tableIDs []uuid.UUID) {
	t.Helper()
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute, CapacitySeats: 100}
	first, victim, last = h.midLoopSeed(t)
	for _, b := range []*domain.Booking{first, victim, last} {
		if err := h.capacity.Create(context.Background(), buildCapacityHolds(b, policy, time.Now())); err != nil {
			t.Fatalf("seed holds for %s: %v", b.ID, err)
		}
	}
	for _, name := range []string{"T1", "T2", "T3"} {
		tableIDs = append(tableIDs, h.addTable(t, name, 4))
	}
	return first, victim, last, tableIDs
}

// TestSeatsToTablesSwitchGivesNoActiveLinkToABookingCancelledBeforeItsWrite is
// the 0058 mirror of the first test: the cancel commits before the placement is
// inserted, so the BEFORE INSERT trigger derives active=false and the table
// stays free.
func TestSeatsToTablesSwitchGivesNoActiveLinkToABookingCancelledBeforeItsWrite(t *testing.T) {
	h := newMidLoopHarness(t, "seats", 100)
	ctx := context.Background()
	first, victim, last, _ := h.midLoopSeedSeats(t)

	h.linkHook.before = func(id uuid.UUID) {
		if id == victim.ID {
			h.cancelOutOfBand(t, victim.ID)
		}
	}

	if _, err := h.midPolicy.Update(ctx, h.manager, h.restaurantID, toTables()); err != nil {
		t.Fatalf("switch to tables: %v", err)
	}

	if got := h.status(t, victim.ID); got != domain.BookingCancelled {
		t.Fatalf("victim status = %q, want cancelled: the out-of-band cancel did not take effect", got)
	}
	if len(h.links(t, victim.ID)) == 0 {
		t.Fatalf("the switch wrote no placement at all for the cancelled booking; this test's premise is gone")
	}

	if act := h.activeLinks(t, victim.ID); len(act) != 0 {
		t.Errorf("INVARIANT BROKEN: a booking cancelled mid-switch came out holding %d ACTIVE table links (%+v). The GiST exclusion constraint on booking_tables sees those rows, so that table is blocked for a guest who is not coming — a table the venue can never sell again for that slot. Migration 0058's BEFORE INSERT trigger is what prevents it.",
			len(act), act)
	}
	// The consequence, measured rather than reasoned about: the table the victim
	// was placed at must be bookable by somebody else in the same slot.
	h.mustBeAbleToSeatSomeoneElse(t, victim)

	for _, b := range []*domain.Booking{first, last} {
		if act := h.activeLinks(t, b.ID); len(act) == 0 {
			t.Errorf("live booking %s came out of the switch with no active placement: its table is invisible to the exclusion constraint and will be sold twice", b.ID)
		}
		if hs := h.holds(t, b.ID); len(hs) != 0 {
			t.Errorf("live booking %s still holds %d capacity rows in tables mode: stale ledger occupancy", b.ID, len(hs))
		}
	}
}

// TestSeatsToTablesSwitchGivesNoActiveLinkToABookingCancelledAfterItsWrite is
// the harder half: the placement is already INSERTed (uncommitted) when the
// cancel commits, so the status trigger of 0004 has no visible row to flip.
func TestSeatsToTablesSwitchGivesNoActiveLinkToABookingCancelledAfterItsWrite(t *testing.T) {
	t.Skip(skipOpenHole)
	h := newMidLoopHarness(t, "seats", 100)
	ctx := context.Background()
	first, victim, last, _ := h.midLoopSeedSeats(t)

	h.linkHook.after = func(id uuid.UUID) {
		if id == victim.ID {
			h.cancelOutOfBand(t, victim.ID)
		}
	}

	if _, err := h.midPolicy.Update(ctx, h.manager, h.restaurantID, toTables()); err != nil {
		t.Fatalf("switch to tables: %v", err)
	}

	if got := h.status(t, victim.ID); got != domain.BookingCancelled {
		t.Fatalf("victim status = %q, want cancelled: the out-of-band cancel did not take effect", got)
	}
	if len(h.links(t, victim.ID)) == 0 {
		t.Fatalf("the switch wrote no placement at all for the cancelled booking; this test's premise is gone")
	}

	if act := h.activeLinks(t, victim.ID); len(act) != 0 {
		t.Errorf("INVARIANT BROKEN: a booking cancelled after its placement was written (but before the switch committed) came out holding %d ACTIVE table links (%+v) — a table blocked for a guest who is not coming.",
			len(act), act)
	}
	h.mustBeAbleToSeatSomeoneElse(t, victim)

	for _, b := range []*domain.Booking{first, last} {
		if act := h.activeLinks(t, b.ID); len(act) == 0 {
			t.Errorf("live booking %s came out of the switch with no active placement", b.ID)
		}
	}
}

// mustBeAbleToSeatSomeoneElse takes the tables the cancelled booking was placed
// at and tries to seat a NEW party there in the same slot. The GiST exclusion
// constraint is the authority in tables mode, so this is the difference between
// "the row says inactive" and "the venue can actually sell that table".
func (h *midLoopHarness) mustBeAbleToSeatSomeoneElse(t *testing.T, victim *domain.Booking) {
	t.Helper()
	placed := h.links(t, victim.ID)
	if len(placed) == 0 {
		return
	}
	replacement := h.seedBooking(t, victim.StartsAt, 4, domain.BookingConfirmed)
	links := make([]domain.BookingTable, 0, len(placed))
	for _, p := range placed {
		links = append(links, domain.BookingTable{
			ID: uuid.New(), BookingID: replacement.ID, TableID: p.TableID,
			SlotStart: p.SlotStart, SlotEnd: p.SlotEnd, CreatedAt: time.Now(),
		})
	}
	if err := bookingrepo.NewTables(h.pool).Create(context.Background(), links); err != nil {
		t.Errorf("INVARIANT BROKEN: the table(s) of the booking cancelled mid-switch cannot be sold to anybody else in that slot (%v) — the cancelled guest is still occupying them", err)
	}
}

// status reads the booking's committed status straight from the row.
func (h *guaranteeHarness) status(t *testing.T, bookingID uuid.UUID) domain.BookingStatus {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM bookings WHERE id=$1`, bookingID).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return domain.BookingStatus(s)
}
