package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// The three holes review found in the table-less capacity guarantee, each
// reproduced against real Postgres. They are here rather than next to the unit
// tests because every one of them is decided by the database — the status
// trigger, the reactivation branch, the bucket CHECK — and a fake can happily
// agree with code Postgres rejects.

// guaranteeHarness wires the real policy / update / create usecases over one
// venue, in whichever capacity mode the test needs.
type guaranteeHarness struct {
	pool     *pgxpool.Pool
	policy   PolicyUseCase
	update   UpdateUseCase
	create   CreateUseCase
	bookings domain.BookingRepository
	capacity domain.BookingCapacityRepository
	txm      domain.TxManager
	restRepo *restrepo.Repository
	related  scheduleReader

	restaurantID uuid.UUID
	manager      Actor
	guest        Actor
}

func newGuaranteeHarness(t *testing.T, mode string, seats int) *guaranteeHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, overrideTables...)
	ctx := context.Background()

	rid := uuid.New()
	var err error
	if mode == "seats" {
		_, err = pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category, is_active,
			                          booking_capacity_mode, booking_capacity_seats)
			 VALUES ($1,'R','Алматы','₸',true,'seats',$2)`, rid, seats)
	} else {
		_, err = pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category, is_active)
			 VALUES ($1,'R','Алматы','₸',true)`, rid)
	}
	if err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'00:00','23:59',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	managerID, guestID := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{managerID, guestID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'U')`,
			id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	t.Cleanup(func() {
		testdb.Truncate(t, pool, overrideTables...)
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurant_tables WHERE restaurant_id=$1`, rid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurant_working_hours WHERE restaurant_id=$1`, rid)
		for _, id := range []uuid.UUID{managerID, guestID} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurants WHERE id=$1`, rid)
	})

	txm := sqltx.NewManager(pool)
	rr := restrepo.New(pool)
	related := restrepo.NewRelated(pool)
	capacity := bookingrepo.NewCapacity(pool)
	bookings := bookingrepo.New(pool)
	managers := newFakeManagers([2]uuid.UUID{managerID, rid})

	return &guaranteeHarness{
		pool: pool, bookings: bookings, capacity: capacity, txm: txm,
		restRepo: rr, related: related,
		policy: NewPolicyUseCase(rr, rr, managers, related, capacity, bookingrepo.NewTables(pool),
			bookings, txm, testConfig()),
		update: NewUpdateUseCase(bookings, bookingrepo.NewTables(pool), capacity,
			bookingrepo.NewOutbox(pool), rr, related, managers, txm, testConfig()),
		create: NewCreateUseCase(bookings, bookingrepo.NewTables(pool), capacity,
			bookingrepo.NewItems(pool), bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
			bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool), rr, related,
			managers, txm, testConfig()),
		restaurantID: rid,
		manager:      Actor{UserID: managerID, Role: domain.RoleRestaurant},
		guest:        Actor{UserID: guestID, Role: domain.RoleUser},
	}
}

// seedBooking writes a booking row directly: these tests are about what the DB
// does to an EXISTING booking, and going through the create usecase would drag
// in availability rules that are not what is under test.
func (h *guaranteeHarness) seedBooking(t *testing.T, start time.Time, guests int, status domain.BookingStatus) *domain.Booking {
	t.Helper()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: h.restaurantID, Name: "Гость",
		Phone: "+7 (777) 000-00-00", PhoneNormalized: "+77770000000",
		Guests: guests, StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.BookingPending, Source: domain.SourceAdmin,
	}
	if err := h.bookings.Create(context.Background(), b); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	if status != domain.BookingPending {
		if err := h.bookings.UpdateStatus(context.Background(), b.ID, status, time.Now()); err != nil {
			t.Fatalf("seed booking status %s: %v", status, err)
		}
		b.Status = status
	}
	return b
}

// slotAt returns a start time two days out at `hour` LOCAL time. The venue is
// open round the clock in these tests, but a visit must still END inside the
// opening hours, so an hour picked in UTC can silently fall over midnight local
// and make an amendment fail for a reason that has nothing to do with capacity.
func (h *guaranteeHarness) slotAt(t *testing.T, hour int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)
	return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, loc).UTC()
}

func (h *guaranteeHarness) holds(t *testing.T, bookingID uuid.UUID) []domain.BookingCapacityHold {
	t.Helper()
	out, err := h.capacity.ListByBooking(context.Background(), bookingID)
	if err != nil {
		t.Fatalf("list holds: %v", err)
	}
	return out
}

func (h *guaranteeHarness) peak(t *testing.T) (taken, limit int) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(),
		`SELECT coalesce(max(seats_taken),0), coalesce(max(seats_limit),0)
		 FROM restaurant_capacity_buckets WHERE restaurant_id=$1`, h.restaurantID).Scan(&taken, &limit); err != nil {
		t.Fatalf("read buckets: %v", err)
	}
	return taken, limit
}

// BLOCKING 1a. A waitlisted booking counts nothing, so the venue is free to
// lower its capacity underneath it. Confirming it later is a NEW claim on the
// room and must be judged by the capacity the venue declares TODAY — not by the
// limit frozen into its hold rows back when it was booked.
//
// Reviewer's reproduction, verbatim: 10-seat venue → booking of 4 waitlisted →
// capacity lowered to 5 → another booking fills 5/5 → confirm the waitlisted one
// → the bucket read 9 seats taken in a five-seat room, silently.
func TestWaitlistedBookingCannotResurrectAnOldCapacity(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 12)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 10}

	// A party of 4 takes its seats, then goes on the waiting list and gives
	// them back.
	waitlisted := h.seedBooking(t, start, 4, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(waitlisted, policy, time.Now())); err != nil {
		t.Fatalf("holds: %v", err)
	}
	if err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingWaitlist, time.Now()); err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	if taken, _ := h.peak(t); taken != 0 {
		t.Fatalf("a waitlisted booking still holds %d seats, want 0", taken)
	}

	// The venue shrinks to 5 seats. Nothing blocks it: nothing is held.
	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(5)); err != nil {
		t.Fatalf("lower capacity: %v", err)
	}
	// And those 5 seats are then sold to somebody else.
	filler := h.seedBooking(t, start, 5, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(filler,
		domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 5}, time.Now())); err != nil {
		t.Fatalf("fill the venue: %v", err)
	}
	if taken, limit := h.peak(t); taken != 5 || limit != 5 {
		t.Fatalf("bucket = %d/%d, want 5/5", taken, limit)
	}

	// Now confirm the waitlisted booking. There is no room; it must fail.
	err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingConfirmed, time.Now())
	if err == nil {
		taken, limit := h.peak(t)
		t.Fatalf("confirming the waitlisted booking succeeded and left the bucket at %d/%d in a 5-seat venue",
			taken, limit)
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("confirm = %v, want the capacity conflict", err)
	}
	if taken, limit := h.peak(t); taken != 5 || limit != 5 {
		t.Fatalf("after the refused confirm the bucket = %d/%d, want 5/5 untouched", taken, limit)
	}
}

// The same hole, reached by the other door. The test above is saved by the
// backfill re-stamping the waitlisted booking's holds when the capacity changes
// through the admin API — so it does NOT prove that the reactivation itself is
// safe. Here the capacity is lowered BY HAND, straight on the row, which
// migration 0054 explicitly expects ("this is exactly the kind of row an
// operator edits by hand") and which re-stamps nothing. The stale limit on the
// hold is then the only number available, and trusting it is the bug.
//
// Revert the reactivation branch of sync_capacity_bucket to NEW.seats_limit and
// this test fails.
func TestWaitlistReactivationIsJudgedByTodaysCapacity(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 12)
	policy10 := domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 10}

	waitlisted := h.seedBooking(t, start, 4, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(waitlisted, policy10, time.Now())); err != nil {
		t.Fatalf("holds: %v", err)
	}
	if err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingWaitlist, time.Now()); err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	// Hand-edited capacity: no usecase, no backfill, the holds keep saying 10.
	if _, err := h.pool.Exec(ctx,
		`UPDATE restaurants SET booking_capacity_seats=5 WHERE id=$1`, h.restaurantID); err != nil {
		t.Fatalf("hand-lower the capacity: %v", err)
	}
	filler := h.seedBooking(t, start, 5, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(filler,
		domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 5}, time.Now())); err != nil {
		t.Fatalf("fill the venue: %v", err)
	}
	if hs := h.holds(t, waitlisted.ID); len(hs) == 0 || hs[0].SeatsLimit != 10 {
		t.Fatalf("the waitlisted holds no longer carry the stale limit, the test proves nothing: %+v", hs)
	}

	err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingConfirmed, time.Now())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		taken, limit := h.peak(t)
		t.Fatalf("confirm = %v, want the capacity conflict; bucket left at %d/%d in a 5-seat venue",
			err, taken, limit)
	}
	if taken, limit := h.peak(t); taken != 5 || limit != 5 {
		t.Fatalf("after the refused confirm the bucket = %d/%d, want 5/5", taken, limit)
	}
}

// And the variant my own 0056 introduced: an OVERBOOKED booking's holds carry a
// limit deliberately raised above the venue. If reactivation trusted that
// number, waitlisting such a booking and confirming it again would silently
// re-claim the extra seats — a second overbooking with no second decision and no
// second audit row. Reachable entirely through the product API.
func TestWaitlistedOverrideCannotReclaimItsExtraSeats(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 12)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 10}

	// 8 sold, then 4 seated anyway: holds stamped at 12 in a 10-seat room.
	sold := h.seedBooking(t, start, 8, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(sold, policy, time.Now())); err != nil {
		t.Fatalf("seed occupancy: %v", err)
	}
	over := h.seedBooking(t, start, 4, domain.BookingPending)
	overHolds := buildCapacityHolds(over, policy, time.Now())
	for i := range overHolds {
		overHolds[i].SeatsLimit = 12
	}
	if err := h.capacity.Create(ctx, overHolds); err != nil {
		t.Fatalf("override holds: %v", err)
	}
	if taken, limit := h.peak(t); taken != 12 || limit != 12 {
		t.Fatalf("bucket after the override = %d/%d, want 12/12", taken, limit)
	}

	// The overbooked party goes on the waiting list, giving its 4 seats back...
	if err := h.bookings.UpdateStatus(ctx, over.ID, domain.BookingWaitlist, time.Now()); err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	// ...the original party of 8 cancels, emptying the room...
	if err := h.bookings.UpdateStatus(ctx, sold.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// ...and the venue legitimately sells 8 of its 10 real seats to somebody
	// else. The room now holds 8 under a limit of 10.
	filler := h.seedBooking(t, start, 8, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(filler, policy, time.Now())); err != nil {
		t.Fatalf("sell the seats again: %v", err)
	}
	if taken, limit := h.peak(t); taken != 8 || limit != 10 {
		t.Fatalf("bucket = %d/%d, want 8/10", taken, limit)
	}

	// Confirming the overbooked booking again is a NEW overbooking and needs a
	// new deliberate decision — it must not ride in on its old raised limit.
	err := h.bookings.UpdateStatus(ctx, over.ID, domain.BookingConfirmed, time.Now())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		taken, limit := h.peak(t)
		t.Fatalf("re-confirm of an overbooked booking = %v, want the conflict; bucket at %d/%d", err, taken, limit)
	}
	var audits int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_capacity_overrides WHERE restaurant_id=$1`, h.restaurantID).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 0 {
		t.Fatalf("audit rows = %d: an overbooking happened that nobody decided", audits)
	}
}

// BLOCKING 1b. A booking waitlisted BEFORE a tables→seats switch used to get no
// holds from the backfill at all, so confirming it flipped zero rows: a
// confirmed booking holding nothing, invisible to the ledger for its whole life.
// It must be backfilled INACTIVE — costing nothing while it waits — and start
// counting the moment it is confirmed.
func TestModeSwitchBackfillsWaitlistedBookings(t *testing.T) {
	h := newGuaranteeHarness(t, "tables", 0)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity) VALUES ($1,$2,'T1',4)`,
		uuid.New(), h.restaurantID); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	start := h.slotAt(t, 12)
	waitlisted := h.seedBooking(t, start, 4, domain.BookingWaitlist)

	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(10)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	holds := h.holds(t, waitlisted.ID)
	if len(holds) == 0 {
		t.Fatal("the waitlisted booking got no holds from the backfill — confirming it would flip nothing")
	}
	for _, hold := range holds {
		if hold.Active {
			t.Fatalf("a waitlisted booking's hold is active: %+v", hold)
		}
	}
	if taken, _ := h.peak(t); taken != 0 {
		t.Fatalf("waiting-list holds cost %d seats, want 0", taken)
	}

	// Confirming it now claims the seats, visibly.
	if err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingConfirmed, time.Now()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if taken, limit := h.peak(t); taken != 4 || limit != 10 {
		t.Fatalf("after the confirm the bucket = %d/%d, want 4/10 — the seats must be counted", taken, limit)
	}
}

// staleRestaurants serves a FROZEN copy of the venue on the first read and the
// real row afterwards. That is exactly the shape of the race under test: the
// usecase reads the policy outside its transaction, somebody commits a change,
// and the transaction then has to notice. Deterministic on purpose — a
// goroutine version of this reproduces the bug only sometimes.
type staleRestaurants struct {
	inner restaurantReader
	stale *domain.RestaurantAggregate
	used  bool
}

func (s *staleRestaurants) GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if !s.used {
		s.used = true
		return s.stale, nil
	}
	return s.inner.GetByID(ctx, id)
}

// BLOCKING 2 (i). PATCH is a capacity writer and must take the venue lock and
// re-read the policy inside its transaction, exactly as create does since
// 709a992. Otherwise a capacity lowered while the request is in flight is
// re-stamped back onto the buckets by this booking's holds and the room is
// oversold.
func TestUpdateRefusesToRestampALoweredCapacity(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 80)
	ctx := context.Background()
	start := h.slotAt(t, 12)
	policy80 := domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 80}

	// 60 of the 80 seats are already sold over the target window.
	other := h.seedBooking(t, start, 60, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(other, policy80, time.Now())); err != nil {
		t.Fatalf("seed occupancy: %v", err)
	}
	// The booking being amended sits well away from it.
	mover := h.seedBooking(t, start.Add(6*time.Hour), 30, domain.BookingConfirmed)
	if err := h.capacity.Create(ctx, buildCapacityHolds(mover, policy80, time.Now())); err != nil {
		t.Fatalf("seed mover: %v", err)
	}

	// The usecase reads a venue that still claims 100 seats; the database says
	// 80. Moving 30 guests into a window already holding 60 fits under the
	// stale 100 and does not fit under the real 80.
	agg, err := h.restRepo.GetByID(ctx, h.restaurantID)
	if err != nil {
		t.Fatalf("read venue: %v", err)
	}
	stale := *agg
	stale.Restaurant.BookingPolicy.BookingCapacitySeats = iptr(100)
	uc := NewUpdateUseCase(h.bookings, bookingrepo.NewTables(h.pool), h.capacity,
		bookingrepo.NewOutbox(h.pool), &staleRestaurants{inner: h.restRepo, stale: &stale},
		h.related, newFakeManagers([2]uuid.UUID{h.manager.UserID, h.restaurantID}),
		h.txm, testConfig())

	moved := start
	_, err = uc.Update(ctx, h.manager, mover.ID, UpdateInput{StartsAt: &moved})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		taken, limit := h.peak(t)
		t.Fatalf("move under a stale capacity = %v, want the conflict; bucket left at %d/%d", err, taken, limit)
	}
	if taken, limit := h.peak(t); taken != 60 || limit != 80 {
		t.Fatalf("bucket = %d/%d, want 60/80 — the venue seats 80, whatever the request had read", taken, limit)
	}
}

// BLOCKING 2 (ii). If the venue switched tables→seats between the policy read
// and the transaction, the amendment resolved the wrong KIND of placement: it
// would write table links and leave the booking's capacity holds sitting at its
// old time. Refuse and let the client retry under the new mode.
func TestUpdateRefusesWhenTheModeChangedMidFlight(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 20)
	ctx := context.Background()
	start := h.slotAt(t, 12)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 20}

	b := h.seedBooking(t, start, 4, domain.BookingConfirmed)
	if err := h.capacity.Create(ctx, buildCapacityHolds(b, policy, time.Now())); err != nil {
		t.Fatalf("seed holds: %v", err)
	}
	before := h.holds(t, b.ID)

	// The usecase reads a venue that still books by tables; the database says
	// seats. Nothing about the resolved placement is usable.
	agg, err := h.restRepo.GetByID(ctx, h.restaurantID)
	if err != nil {
		t.Fatalf("read venue: %v", err)
	}
	stale := *agg
	stale.Restaurant.BookingPolicy.BookingCapacityMode = capModePtr(domain.CapacityModeTables)
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity) VALUES ($1,$2,'T1',8)`,
		uuid.New(), h.restaurantID); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	uc := NewUpdateUseCase(h.bookings, bookingrepo.NewTables(h.pool), h.capacity,
		bookingrepo.NewOutbox(h.pool), &staleRestaurants{inner: h.restRepo, stale: &stale},
		h.related, newFakeManagers([2]uuid.UUID{h.manager.UserID, h.restaurantID}),
		h.txm, testConfig())

	moved := start.Add(4 * time.Hour)
	if _, err := uc.Update(ctx, h.manager, b.ID, UpdateInput{StartsAt: &moved}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("move across a mode change = %v, want the retry conflict", err)
	}
	// Nothing moved: the holds are still where they were, and so is the booking.
	after := h.holds(t, b.ID)
	if len(after) != len(before) || !after[0].BucketStart.Equal(before[0].BucketStart) {
		t.Fatalf("holds moved despite the refusal: %v → %v", before[0].BucketStart, after[0].BucketStart)
	}
	reloaded, err := h.bookings.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.StartsAt.Equal(start) {
		t.Fatalf("booking moved to %s despite the refusal", reloaded.StartsAt)
	}
}

// GAP. The mode-switch backfill reaches BACK one occupancy window, because a
// booking that started before the switch but has not finished still occupies the
// room. Without that lookback the party sitting in the venue at the moment of
// the switch is invisible to the new engine and its seats are sold a second
// time. Revert `from` to time.Now() in rebuildHolds and this test fails.
func TestModeSwitchBackfillsAnInProgressBooking(t *testing.T) {
	h := newGuaranteeHarness(t, "tables", 0)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity) VALUES ($1,$2,'T1',40)`,
		uuid.New(), h.restaurantID); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	// A party of 30 sat down half an hour ago and stays for two hours.
	seated := h.seedBooking(t, time.Now().Add(-30*time.Minute).UTC(), 30, domain.BookingArrived)

	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(40)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	if len(h.holds(t, seated.ID)) == 0 {
		t.Fatal("the party already seated got no holds: the switch declared the room empty while 30 people sat in it")
	}
	if taken, _ := h.peak(t); taken != 30 {
		t.Fatalf("seats_taken after the switch = %d, want 30", taken)
	}
	// And the consequence that makes it a bug rather than a detail: the room has
	// 10 seats left, not 40.
	over := h.seedBooking(t, time.Now().UTC(), 11, domain.BookingPending)
	err := h.capacity.Create(ctx, buildCapacityHolds(over,
		domain.BookingPolicy{Duration: 2 * time.Hour, CapacitySeats: 40}, time.Now()))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("selling 11 more seats of the 10 left = %v, want ErrAlreadyExists", err)
	}
}

// links reads the booking's table links straight from the repository, including
// the inactive ones — "does this booking hold a table at all?" is exactly what
// the reconciliation below has to answer.
func (h *guaranteeHarness) links(t *testing.T, bookingID uuid.UUID) []domain.BookingTable {
	t.Helper()
	out, err := bookingrepo.NewTables(h.pool).ListByBooking(context.Background(), bookingID)
	if err != nil {
		t.Fatalf("list booking tables: %v", err)
	}
	return out
}

// addTable gives the venue one active table and returns its id.
func (h *guaranteeHarness) addTable(t *testing.T, name string, capacity int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity) VALUES ($1,$2,$3,$4)`,
		id, h.restaurantID, name, capacity); err != nil {
		t.Fatalf("seed table %s: %v", name, err)
	}
	return id
}

// toTables is the PATCH that moves the venue back to booking by tables.
func toTables() domain.BookingPolicyOverride {
	return domain.BookingPolicyOverride{BookingCapacityMode: capModePtr(domain.CapacityModeTables)}
}

// BLOCKING (review, 25.07.2026). The seats→tables direction of the mode switch
// reconciled NOTHING: it checked that the venue had at least one active table
// and wrote the policy. A capacity-mode booking owns no booking_tables row (see
// create.go), and the table engine reads only booking_tables — so the moment the
// venue was back in table mode, every table looked free at a time a live
// table-less booking was already sitting on, and the same seats were sold twice.
//
// The whole sequence is ordinary product usage: book table-lessly, add a table,
// flip the mode, let the next guest book the same slot.
func TestModeSwitchBackToTablesSeatsTheCapacityBookings(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 19)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute, CapacitySeats: 10}

	// 1. Eight of the ten seats are sold table-lessly and confirmed.
	sold := h.seedBooking(t, start, 8, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(sold, policy, time.Now())); err != nil {
		t.Fatalf("seed holds: %v", err)
	}
	if err := h.bookings.UpdateStatus(ctx, sold.ID, domain.BookingConfirmed, time.Now()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := h.links(t, sold.ID); len(got) != 0 {
		t.Fatalf("a capacity-mode booking already holds %d table links, the test premise is wrong", len(got))
	}

	// 2. The owner writes down the one table that room actually is, and switches
	//    the venue back to booking by tables.
	h.addTable(t, "T1", 10)
	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, toTables()); err != nil {
		t.Fatalf("switch back to tables: %v", err)
	}

	// The live booking must now hold a REAL table: booking_tables and its
	// exclusion constraint are the only guarantee left in this mode.
	placed := h.links(t, sold.ID)
	if len(placed) == 0 {
		t.Fatal("the confirmed table-less booking holds no table after the switch: the room reads empty at 19:00")
	}
	for _, l := range placed {
		if !l.Active {
			t.Fatalf("the placement of a confirmed booking is inactive: %+v", l)
		}
		if !l.SlotStart.Equal(start.Add(-15*time.Minute)) || !l.SlotEnd.Equal(start.Add(2*time.Hour+15*time.Minute)) {
			t.Fatalf("placement slot = %s..%s, want the booking's own occupancy window",
				l.SlotStart, l.SlotEnd)
		}
	}
	// And it no longer holds seats: the seats ledger does not govern this venue
	// any more, and a hold nobody maintains is a hold that lies.
	if hs := h.holds(t, sold.ID); len(hs) != 0 {
		t.Fatalf("the booking still holds %d capacity rows after leaving seats mode", len(hs))
	}

	// 3. The consequence that makes it a bug: the next guest books the same
	//    19:00 through the ordinary table flow. It must be refused.
	_, err := h.create.Create(ctx, h.guest, CreateInput{
		RestaurantID: h.restaurantID, UserID: &h.guest.UserID,
		Name: "Второй гость", Phone: "+7 (701) 111-11-11",
		Guests: 4, StartsAt: start, Source: domain.SourceApp,
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second booking onto the same seats = %v, want ErrAlreadyExists", err)
	}
}

// The other half of the same fix: a booking the new table list cannot seat must
// refuse the whole switch, with the venue left exactly as it was. The
// alternative shapes are both worse — leaving it unseated is the bug, and
// cancelling a confirmed guest to make a settings toggle succeed is not the
// backend's decision.
func TestModeSwitchBackToTablesRefusedWhenABookingCannotBeSeated(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 19)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute, CapacitySeats: 10}

	sold := h.seedBooking(t, start, 8, domain.BookingPending)
	if err := h.capacity.Create(ctx, buildCapacityHolds(sold, policy, time.Now())); err != nil {
		t.Fatalf("seed holds: %v", err)
	}
	// The venue's whole table list seats four, and eight guests are coming.
	h.addTable(t, "T1", 4)

	_, err := h.policy.Update(ctx, h.manager, h.restaurantID, toTables())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("switch that strands a confirmed party = %v, want ErrValidation", err)
	}

	// Nothing moved: still seats mode, still holding its seats, still no links.
	view, err := h.policy.Get(ctx, h.manager, h.restaurantID)
	if err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if view.Effective.CapacityMode != domain.CapacityModeSeats {
		t.Fatalf("mode after the refused switch = %q, want seats", view.Effective.CapacityMode)
	}
	if got := h.links(t, sold.ID); len(got) != 0 {
		t.Fatalf("the refused switch left %d table links behind", len(got))
	}
	if hs := h.holds(t, sold.ID); len(hs) == 0 {
		t.Fatal("the refused switch dropped the booking's capacity holds")
	}
	if taken, _ := h.peak(t); taken != 8 {
		t.Fatalf("seats taken after the refused switch = %d, want 8", taken)
	}
}

// A booking on the WAITING LIST is placed too, and its placement is inactive
// while it waits — the mirror of what 0057 does with capacity holds. Two things
// have to hold at once: waiting costs the venue nothing, and confirming later is
// judged by the exclusion constraint instead of slipping past it (a waitlisted
// booking with no links at all would come back confirmed and invisible).
func TestModeSwitchBackToTablesPlacesWaitlistedBookingsInactive(t *testing.T) {
	h := newGuaranteeHarness(t, "seats", 10)
	ctx := context.Background()
	start := h.slotAt(t, 19)

	waitlisted := h.seedBooking(t, start, 4, domain.BookingWaitlist)
	h.addTable(t, "T1", 4)
	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, toTables()); err != nil {
		t.Fatalf("switch back to tables: %v", err)
	}

	placed := h.links(t, waitlisted.ID)
	if len(placed) == 0 {
		t.Fatal("the waitlisted booking got no placement — confirming it would flip nothing")
	}
	for _, l := range placed {
		if l.Active {
			t.Fatalf("a waitlisted booking occupies a table: %+v", l)
		}
	}

	// The table is therefore still sellable, and somebody else buys it.
	if _, err := h.create.Create(ctx, h.guest, CreateInput{
		RestaurantID: h.restaurantID, UserID: &h.guest.UserID,
		Name: "Второй гость", Phone: "+7 (701) 222-22-22",
		Guests: 4, StartsAt: start, Source: domain.SourceApp,
	}); err != nil {
		t.Fatalf("the waiting list must not block the table: %v", err)
	}

	// Confirming the waitlisted booking now re-claims a table that is gone. The
	// DATABASE is what refuses it.
	err := h.bookings.UpdateStatus(ctx, waitlisted.ID, domain.BookingConfirmed, time.Now())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("confirm into a taken table = %v, want ErrAlreadyExists", err)
	}
}

// seatAt gives the booking a real placement at `tableID` over its own occupancy
// window, the way an ordinary table-mode create would. Written through the
// repository (not raw SQL) so migration 0058 derives `active` from the booking's
// status exactly as it does in production.
func (h *guaranteeHarness) seatAt(t *testing.T, b *domain.Booking, tableID uuid.UUID, start time.Time) {
	t.Helper()
	from, to := occupancyWindow(start, domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute})
	if err := bookingrepo.NewTables(h.pool).Create(context.Background(), []domain.BookingTable{{
		ID: uuid.New(), BookingID: b.ID, TableID: tableID,
		SlotStart: from, SlotEnd: to, CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("seat booking at table: %v", err)
	}
}

// availability builds the real read-only seats/tables availability engine over
// the same venue. Used where the question is not "what is in the ledger" but
// "what does the engine that sells the next booking believe".
func (h *guaranteeHarness) availability() AvailabilityUseCase {
	return NewAvailabilityUseCase(bookingrepo.NewTables(h.pool), h.capacity, h.restRepo, h.related, testConfig())
}

// slotOn returns the slot of `start` from the venue's own availability answer.
func (h *guaranteeHarness) slotOn(t *testing.T, start time.Time, guests int) Slot {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day, err := h.availability().Day(context.Background(), h.restaurantID,
		start.In(loc).Format(DateLayout), guests)
	if err != nil {
		t.Fatalf("availability: %v", err)
	}
	for _, s := range day.Slots {
		if s.StartsAt.Equal(start) {
			return s
		}
	}
	t.Fatalf("availability returned no slot at %s (got %d slots)", start, len(day.Slots))
	return Slot{}
}

// BLOCKING (re-review of b02d815, 25.07.2026). The ROUND TRIP leaks capacity
// holds, and neither mode-switch test above covered it.
//
// A booking created in TABLES mode owns a real booking_tables link. The
// tables → seats direction (rebuildHolds) gives it capacity holds and
// deliberately does NOT touch that link — it must not, see the guard test below.
// So while the venue is table-less such a booking is legitimately recorded in
// both tables: the ledger is the authority, the leftover link is inert because
// nothing in seats mode reads booking_tables.
//
// Switching BACK to tables was where it broke: seatCapacityBookings saw
// len(own) > 0, `continue`d — and skipped the ReplaceForBooking(nil) that drops
// the holds. The booking kept counting against a seats ledger that no longer
// governs the venue: restaurant_capacity_buckets reported phantom occupancy for
// its window until it reached a terminal status or a third switch overwrote it.
//
// Not a double-sell (a table-mode create never reads the ledger), but the bucket
// counters feed reports and the capacity guard of any later switch, so a phantom
// there is a lie the product acts on.
func TestModeSwitchRoundTripDropsTheStaleCapacityHolds(t *testing.T) {
	h := newGuaranteeHarness(t, "tables", 0)
	ctx := context.Background()
	start := h.slotAt(t, 19)

	// 1. Tables mode: a confirmed booking of four sitting at a real table.
	tableID := h.addTable(t, "T1", 4)
	b := h.seedBooking(t, start, 4, domain.BookingConfirmed)
	h.seatAt(t, b, tableID, start)

	// 2. The venue goes table-less. The booking is backfilled into the ledger
	//    and keeps its (now inert) placement — both are correct, see below.
	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(10)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}
	if hs := h.holds(t, b.ID); len(hs) == 0 {
		t.Fatal("the tables-origin booking got no holds in seats mode: the room would read empty")
	}
	if got := h.links(t, b.ID); len(got) == 0 {
		t.Fatal("the switch to seats dropped the real placement; this test's premise is gone")
	}

	// 3. And back to tables. booking_tables is the authority again, so the
	//    ledger must be left empty for this booking.
	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, toTables()); err != nil {
		t.Fatalf("switch back to tables: %v", err)
	}
	if hs := h.holds(t, b.ID); len(hs) != 0 {
		t.Fatalf("back in tables mode the booking still holds %d capacity rows: the buckets report seats nobody is taking", len(hs))
	}
	if taken, _ := h.peak(t); taken != 0 {
		t.Fatalf("seats_taken after returning to tables mode = %d, want 0 (phantom occupancy)", taken)
	}
	// Its real placement is untouched: the guest was never moved, and the table
	// engine still sees the seats as taken.
	placed := h.links(t, b.ID)
	if len(placed) != 1 || placed[0].TableID != tableID || !placed[0].Active {
		t.Fatalf("the round trip disturbed the real placement: %+v", placed)
	}
}

// The guard that forbids the OTHER fix for the leak above. It is tempting to
// close it at the source — have rebuildHolds skip bookings that already hold
// real active table links, so nothing is ever accounted twice. That opens a
// worse hole, and this test is what fails if anyone tries.
//
// In seats mode the table engine is NOT the authority: availabilityUseCase.Day
// takes the seats branch and reads restaurant_capacity_buckets ONLY (see
// availability.go), and create.go's seats path likewise never calls ListBusy. A
// tables-origin booking with no capacity hold would therefore be invisible while
// the venue is table-less — its guests are in the room, the ledger says the
// seats are free, and the next guest is sold them. Under-counting is a real
// overbooking; the phantom hold the round-trip test guards against is only a
// wrong number in a report. Hence the fix belongs on the seats → tables side.
func TestTablesOriginBookingStillOccupiesSeatsInSeatsMode(t *testing.T) {
	h := newGuaranteeHarness(t, "tables", 0)
	ctx := context.Background()
	start := h.slotAt(t, 19)

	tableID := h.addTable(t, "T1", 4)
	b := h.seedBooking(t, start, 4, domain.BookingConfirmed)
	h.seatAt(t, b, tableID, start)

	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(10)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	// The ledger — the only thing the seats engine reads — must show the four
	// guests already in the room.
	if taken, _ := h.peak(t); taken != 4 {
		t.Fatalf("seats_taken in seats mode = %d, want 4: a tables-origin booking stopped counting", taken)
	}
	// And the engine that sells the next booking must agree: six seats left, so
	// a party of six fits and a party of seven does not.
	if s := h.slotOn(t, start, 6); !s.Available {
		t.Fatalf("a party of 6 into the 6 free seats is refused: %+v", s)
	}
	if s := h.slotOn(t, start, 7); s.Available {
		t.Fatalf("a party of 7 sold into 6 free seats: %+v", s)
	} else if s.RemainingSeats == nil || *s.RemainingSeats != 6 {
		t.Fatalf("remaining seats = %v, want 6", s.RemainingSeats)
	}
}
