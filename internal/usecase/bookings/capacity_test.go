package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// seatsOverride is the stored configuration of a table-less venue.
func seatsOverride(seats int) domain.BookingPolicyOverride {
	return domain.BookingPolicyOverride{
		BookingCapacityMode:  capModePtr(domain.CapacityModeSeats),
		BookingCapacitySeats: iptr(seats),
	}
}

func TestCapacityBucketsCoverTheWholeWindow(t *testing.T) {
	base := time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC)

	// An exact grid window: 19:00–20:00 is four buckets, and 20:00 belongs to
	// the next booking (half-open).
	got := capacityBuckets(base, base.Add(time.Hour))
	if len(got) != 4 || !got[0].Equal(base) || !got[3].Equal(base.Add(45*time.Minute)) {
		t.Fatalf("exact window = %v", got)
	}
	// An off-grid start rounds DOWN, so the partially used quarter-hour is held
	// in full — the safe direction.
	got = capacityBuckets(base.Add(7*time.Minute), base.Add(time.Hour))
	if len(got) != 4 || !got[0].Equal(base) {
		t.Fatalf("off-grid window = %v", got)
	}
	// An off-grid end rounds UP for the same reason.
	got = capacityBuckets(base, base.Add(time.Hour+time.Minute))
	if len(got) != 5 {
		t.Fatalf("off-grid end = %d buckets, want 5", len(got))
	}
	if capacityBuckets(base, base) != nil {
		t.Fatal("an empty window must claim nothing")
	}
}

// The party has to fit for the WHOLE visit: one busy bucket in the middle is
// enough to make the slot unbookable, however free the edges are.
func TestFreeSeatsTakesTheWorstBucket(t *testing.T) {
	base := time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC)
	usage := usageIndex([]domain.CapacityUsage{
		{BucketStart: base.Add(30 * time.Minute), SeatsTaken: 18, SeatsLimit: 20},
	})
	if got := freeSeats(usage, base, base.Add(time.Hour), 20); got != 2 {
		t.Fatalf("freeSeats = %d, want 2 (the worst bucket)", got)
	}
	if got := freeSeats(usage, base, base.Add(15*time.Minute), 20); got != 20 {
		t.Fatalf("freeSeats before the busy bucket = %d, want 20", got)
	}
	// A venue that lowered its capacity below what is already sold reports
	// nothing left, never a negative number.
	over := usageIndex([]domain.CapacityUsage{
		{BucketStart: base, SeatsTaken: 30, SeatsLimit: 20},
	})
	if got := freeSeats(over, base, base.Add(15*time.Minute), 20); got != 0 {
		t.Fatalf("oversold bucket = %d, want 0", got)
	}
}

func newSeatsAvailability(t *testing.T, rid uuid.UUID, seats int, capacity *fakeCapacity) AvailabilityUseCase {
	t.Helper()
	return NewAvailabilityUseCase(
		&fakeLinks{},
		capacity,
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
			ID: rid, IsActive: true, BookingPolicy: seatsOverride(seats),
		}}},
		// No tables at all: the exact state of the 24 venues this mode exists for.
		&fakeSchedule{hours: openAllWeek("12:00", "18:00")},
		testConfig(),
	)
}

// A table-less venue with no tables must produce BOOKABLE slots — the launch
// blocker in one test — and describe the remaining capacity honestly.
func TestAvailabilitySeatsMode(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()
	day := time.Now().In(loc).AddDate(0, 0, 7)
	capacity := newFakeCapacity()
	u := newSeatsAvailability(t, rid, 20, capacity)

	got, err := u.Day(context.Background(), rid, day.Format(DateLayout), 4)
	if err != nil {
		t.Fatalf("Day: %v", err)
	}
	if got.CapacityMode != domain.CapacityModeSeats || got.CapacitySeats != 20 {
		t.Fatalf("day meta = %+v", got)
	}
	if len(got.Slots) == 0 {
		t.Fatal("a venue in capacity mode with no tables must still offer slots")
	}
	for _, s := range got.Slots {
		if !s.Available {
			t.Fatalf("empty venue slot %s is unavailable: %+v", s.StartsAt, s)
		}
		if s.RemainingSeats == nil || *s.RemainingSeats != 20 {
			t.Fatalf("slot %s remaining = %v, want 20", s.StartsAt, s.RemainingSeats)
		}
		// free_tables stays renderable for the shipped client: 5 more parties
		// of 4 fit into 20 seats.
		if s.FreeTables != 5 {
			t.Fatalf("slot %s free_tables = %d, want 5", s.StartsAt, s.FreeTables)
		}
	}

	// A party bigger than the venue is "capacity", not "occupied": it will
	// never fit, and the app should say so.
	got, err = u.Day(context.Background(), rid, day.Format(DateLayout), 25)
	if err != nil {
		t.Fatalf("Day(25): %v", err)
	}
	for _, s := range got.Slots {
		if s.Available || s.Reason != ReasonCapacity {
			t.Fatalf("oversized party slot = %+v, want reason %q", s, ReasonCapacity)
		}
	}
}

// Sold seats have to show up in the calendar, and the slot must flip to
// "occupied" exactly when the party no longer fits.
func TestAvailabilitySeatsModeReflectsSoldSeats(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()
	day := time.Now().In(loc).AddDate(0, 0, 7)
	capacity := newFakeCapacity()
	u := newSeatsAvailability(t, rid, 10, capacity)

	// Somebody already holds 8 of the 10 seats over the 13:00 visit.
	noon := time.Date(day.Year(), day.Month(), day.Day(), 13, 0, 0, 0, loc)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute, CapacitySeats: 10}
	b := &domain.Booking{ID: uuid.New(), RestaurantID: rid, Guests: 8, StartsAt: noon}
	if err := capacity.Create(context.Background(), buildCapacityHolds(b, policy, time.Now())); err != nil {
		t.Fatalf("seed holds: %v", err)
	}

	got, err := u.Day(context.Background(), rid, day.Format(DateLayout), 3)
	if err != nil {
		t.Fatalf("Day: %v", err)
	}
	byClock := map[string]Slot{}
	for _, s := range got.Slots {
		byClock[s.StartsAt.In(loc).Format("15:04")] = s
	}
	s, ok := byClock["13:00"]
	if !ok {
		t.Fatalf("13:00 missing from %v", byClock)
	}
	if s.Available || s.Reason != ReasonOccupied {
		t.Fatalf("13:00 with 8/10 sold and a party of 3 = %+v, want occupied", s)
	}
	if s.RemainingSeats == nil || *s.RemainingSeats != 2 {
		t.Fatalf("13:00 remaining = %v, want 2", s.RemainingSeats)
	}
	// Two guests still fit into the same slot.
	got, err = u.Day(context.Background(), rid, day.Format(DateLayout), 2)
	if err != nil {
		t.Fatalf("Day(2): %v", err)
	}
	for _, s := range got.Slots {
		if s.StartsAt.In(loc).Format("15:04") == "13:00" && !s.Available {
			t.Fatalf("a party of 2 must still fit into the last 2 seats: %+v", s)
		}
	}
}

// newSeatsCreateHarness rebuilds the create harness for a table-less venue.
func newSeatsCreateHarness(t *testing.T, seats int) *createHarness {
	t.Helper()
	h := newCreateHarness(t, seatsOverride(seats))
	// The venue keeps no tables; make the fake schedule say so, otherwise the
	// test would not reproduce the situation the mode exists for.
	h.schedule.tables = nil
	return h
}

// The boundary, at the usecase level: the capacity may be filled exactly, and
// the next guest is refused as a conflict.
func TestCreateSeatsModeHonoursCapacity(t *testing.T) {
	h := newSeatsCreateHarness(t, 6)
	ctx := context.Background()

	in := h.input()
	in.Guests = 4
	if _, err := h.uc.Create(ctx, h.guest, in); err != nil {
		t.Fatalf("first booking: %v", err)
	}
	in.Guests = 2
	if _, err := h.uc.Create(ctx, h.guest, in); err != nil {
		t.Fatalf("booking that fills the venue exactly must be accepted: %v", err)
	}
	in.Guests = 1
	_, err := h.uc.Create(ctx, h.guest, in)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("seat past the capacity = %v, want ErrAlreadyExists", err)
	}
}

// A party the venue could never seat is a validation error, not "we are full":
// the two say different things to a guest.
func TestCreateSeatsModeRejectsOversizedParty(t *testing.T) {
	h := newSeatsCreateHarness(t, 6)
	in := h.input()
	in.Guests = 8
	_, err := h.uc.Create(context.Background(), h.guest, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("party larger than the venue = %v, want ErrValidation", err)
	}
}

// Manual placement makes no sense without tables and must be refused rather
// than silently ignored — a "forced" booking with no hold would be invisible to
// the capacity guard.
func TestCreateSeatsModeRefusesManualPlacement(t *testing.T) {
	h := newSeatsCreateHarness(t, 20)
	ctx := context.Background()

	in := h.input()
	in.TableIDs = []uuid.UUID{uuid.New()}
	if _, err := h.uc.Create(ctx, h.manager, in); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("table pinning in capacity mode = %v, want ErrValidation", err)
	}
	in = h.input()
	in.Force = true
	in.TableIDs = []uuid.UUID{uuid.New()}
	if _, err := h.uc.Create(ctx, h.manager, in); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("forced placement in capacity mode = %v, want ErrValidation", err)
	}
}

// A table-mode venue must be completely unaffected: it keeps seating guests at
// tables and writes no capacity hold at all.
func TestCreateTableModeUnaffectedByCapacityFeature(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	details, err := h.uc.Create(context.Background(), h.guest, h.input())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(details.Tables) == 0 {
		t.Fatal("a table-mode booking must be seated at a table")
	}
	if len(h.capacity.holds) != 0 {
		t.Fatalf("table-mode booking wrote %d capacity holds, want none", len(h.capacity.holds))
	}
}

// --- admin API -------------------------------------------------------------

func TestPolicyUpdateCapacityValidation(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	cases := []struct {
		name  string
		patch domain.BookingPolicyOverride
	}{
		{"zero capacity", seatsOverride(0)},
		{"negative capacity", seatsOverride(-4)},
		{"absurd capacity", seatsOverride(maxCapacitySeats + 1)},
		{"unknown mode", domain.BookingPolicyOverride{
			BookingCapacityMode:  capModePtr(domain.CapacityMode("free-for-all")),
			BookingCapacitySeats: iptr(20),
		}},
		{"capacity mode without a capacity", domain.BookingPolicyOverride{
			BookingCapacityMode: capModePtr(domain.CapacityModeSeats),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.uc.Update(ctx, actor, h.rid, tc.patch); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Update(%s) = %v, want ErrValidation", tc.name, err)
			}
		})
	}
	if h.writer.calls != 0 {
		t.Fatalf("a rejected capacity patch still wrote %d times", h.writer.calls)
	}

	// The upper bound itself is accepted: the limit catches typos, it does not
	// second-guess how big a banquet hall may be.
	if _, err := h.uc.Update(ctx, actor, h.rid, seatsOverride(maxCapacitySeats)); err != nil {
		t.Fatalf("capacity at the bound = %v, want accepted", err)
	}
}

// Switching a venue INTO capacity mode must carry its already accepted bookings
// over, otherwise the new engine would resell their seats.
func TestPolicySwitchToSeatsBackfillsExistingBookings(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	existing := domain.Booking{
		ID: uuid.New(), RestaurantID: h.rid, Guests: 6,
		StartsAt: time.Now().Add(48 * time.Hour).UTC(), Status: domain.BookingConfirmed,
	}
	h.bookings.list = []domain.Booking{existing}
	h.bookings.total = 1

	view, err := h.uc.Update(ctx, actor, h.rid, seatsOverride(20))
	if err != nil {
		t.Fatalf("switch to seats: %v", err)
	}
	if view.Effective.CapacityMode != domain.CapacityModeSeats || view.Effective.CapacitySeats != 20 {
		t.Fatalf("effective policy after switch = %+v", view.Effective)
	}
	holds := h.capacity.holds[existing.ID]
	if len(holds) == 0 {
		t.Fatal("the booking already accepted got no capacity holds")
	}
	for _, hold := range holds {
		if hold.Seats != 6 || hold.SeatsLimit != 20 {
			t.Fatalf("backfilled hold = %+v, want 6 seats under a limit of 20", hold)
		}
	}
	// The filter must pick every booking that will ever need a hold and nothing
	// else: the three statuses that hold a seat now, PLUS waitlist (confirming a
	// waitlisted booking re-claims a seat by flipping holds it must therefore
	// already own — see statusesNeedingHolds), and only recent/future ones — a
	// backfill of last year's cancelled bookings would block the venue.
	got := h.bookings.lastFlt
	if got.From == nil {
		t.Fatalf("backfill filter has no lower bound: %+v", got)
	}
	want := map[domain.BookingStatus]bool{
		domain.BookingPending: false, domain.BookingConfirmed: false,
		domain.BookingArrived: false, domain.BookingWaitlist: false,
	}
	for _, s := range got.Statuses {
		seen, ok := want[s]
		if !ok {
			t.Fatalf("backfill filter includes %q, which never needs a hold", s)
		}
		if seen {
			t.Fatalf("backfill filter repeats %q", s)
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Fatalf("backfill filter misses %q: %+v", s, got.Statuses)
		}
	}
}

// A capacity too small for what the venue has already sold is refused, with the
// switch left unapplied.
func TestPolicySwitchToSeatsRefusedWhenBookingsDoNotFit(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	h.bookings.list = []domain.Booking{{
		ID: uuid.New(), RestaurantID: h.rid, Guests: 12,
		StartsAt: time.Now().Add(48 * time.Hour).UTC(), Status: domain.BookingConfirmed,
	}}
	h.bookings.total = 1

	if _, err := h.uc.Update(ctx, actor, h.rid, seatsOverride(8)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("switch that strands a booking = %v, want ErrValidation", err)
	}
}

// Switching BACK to table mode is only allowed for a venue that actually has
// tables — otherwise the switch would recreate the launch blocker.
func TestPolicySwitchToTablesNeedsTables(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}
	toTables := domain.BookingPolicyOverride{BookingCapacityMode: capModePtr(domain.CapacityModeTables)}

	if _, err := h.uc.Update(ctx, actor, h.rid, toTables); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("switch to tables without tables = %v, want ErrValidation", err)
	}

	h.schedule.tables = []domain.RestaurantTable{table("t4", 4)}
	view, err := h.uc.Update(ctx, actor, h.rid, toTables)
	if err != nil {
		t.Fatalf("switch to tables with tables = %v", err)
	}
	if view.Effective.CapacityMode != domain.CapacityModeTables {
		t.Fatalf("effective policy after switch = %+v", view.Effective)
	}
}

// Lowering the declared capacity below what is already sold for a future moment
// is refused before anything is written.
func TestPolicyLowerCapacityBelowSoldSeats(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	bookingID := uuid.New()
	if err := h.capacity.Create(ctx, []domain.BookingCapacityHold{{
		BookingID: bookingID, RestaurantID: h.rid,
		BucketStart: time.Now().Add(24 * time.Hour).UTC().Truncate(domain.CapacityBucket),
		Seats:       15, SeatsLimit: 20,
	}}); err != nil {
		t.Fatalf("seed hold: %v", err)
	}

	if _, err := h.uc.Update(ctx, actor, h.rid, seatsOverride(10)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("lowering below sold seats = %v, want ErrValidation", err)
	}
	if _, err := h.uc.Update(ctx, actor, h.rid, seatsOverride(16)); err != nil {
		t.Fatalf("lowering to a value that still fits = %v, want accepted", err)
	}
}

// A create must take the venue's capacity lock and re-read the policy INSIDE
// its transaction. Without that, a create that read "capacity 100" outside the
// tx re-stamps buckets a concurrent policy change just lowered to 80 — the
// venue believes 80 and has 98. The fake cannot reproduce the interleaving, so
// what is pinned here is the thing that makes it impossible: the lock is taken,
// and it is taken on THIS venue.
func TestCreateSeatsModeLocksTheVenue(t *testing.T) {
	h := newSeatsCreateHarness(t, 20)
	ctx := context.Background()

	in := h.input()
	in.Guests = 4
	if _, err := h.uc.Create(ctx, h.guest, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(h.capacity.locked) != 1 || h.capacity.locked[0] != in.RestaurantID {
		t.Fatalf("capacity lock taken %v, want exactly [%s] — an unlocked create can be overtaken by a policy change",
			h.capacity.locked, in.RestaurantID)
	}
}
