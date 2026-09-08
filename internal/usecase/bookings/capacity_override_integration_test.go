package bookings

import (
	"context"
	"errors"
	"sync"
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

// Deliberate overbooking against REAL Postgres.
//
// The unit tests next door run over a fake that imitates the two behaviours the
// mechanism rests on. This file exists because the guarantee under change is a
// DB-level one: chk_capacity_bucket_within_limit decides who is refused, and
// apply_capacity_delta's "every increment re-stamps seats_limit" is what makes a
// raised limit un-inheritable. A fake can agree with an idea that Postgres
// rejects; only this file can tell.

// overrideTables are the tables these tests own, children first.
var overrideTables = []string{
	"booking_capacity_overrides", "booking_capacity_holds", "restaurant_capacity_buckets",
	"booking_outbox", "booking_status_history", "booking_rate_log",
	"booking_blacklist", "booking_items", "booking_tables", "bookings",
}

type overrideHarness struct {
	pool     *pgxpool.Pool
	create   CreateUseCase
	bookings domain.BookingRepository
	capacity domain.BookingCapacityRepository

	restaurantID uuid.UUID
	guest        Actor
	manager      Actor
	stranger     Actor
	startsAt     time.Time
}

// newOverrideHarness wires the real create usecase over a table-less venue with
// a declared capacity of `seats` and NO tables at all — the exact shape of the
// venues capacity mode exists for.
func newOverrideHarness(t *testing.T, seats int) *overrideHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, overrideTables...)
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active,
		                          booking_capacity_mode, booking_capacity_seats)
		 VALUES ($1,'Capacity R','Алматы','₸',true,'seats',$2)`, rid, seats); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	guestID, managerID, strangerID := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{guestID, managerID, strangerID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'U')`,
			id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	t.Cleanup(func() {
		// Ordered so the audit rows go before the bookings they reference: the
		// audit FK is RESTRICT and the table refuses a DELETE outright, so this
		// uses TRUNCATE (which fires no row trigger) — the same way the suite
		// starts.
		testdb.Truncate(t, pool, overrideTables...)
		for _, id := range []uuid.UUID{guestID, managerID, strangerID} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurants WHERE id=$1`, rid)
	})

	txm := sqltx.NewManager(pool)
	capacity := bookingrepo.NewCapacity(pool)
	bookings := bookingrepo.New(pool)
	create := NewCreateUseCase(
		bookings, bookingrepo.NewTables(pool), capacity, bookingrepo.NewItems(pool),
		bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool),
		newFakeManagers([2]uuid.UUID{managerID, rid}),
		txm, testConfig(),
	)

	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)
	return &overrideHarness{
		pool: pool, create: create, bookings: bookings, capacity: capacity,
		restaurantID: rid,
		guest:        Actor{UserID: guestID, Role: domain.RoleUser},
		manager:      Actor{UserID: managerID, Role: domain.RoleRestaurant},
		stranger:     Actor{UserID: strangerID, Role: domain.RoleRestaurant},
		startsAt:     time.Date(day.Year(), day.Month(), day.Day(), 19, 0, 0, 0, loc).UTC(),
	}
}

// input builds a request for `guests`. Each call gets a fresh phone so the
// anti-fraud counter (10 attempts per phone per hour) cannot decide a test.
func (h *overrideHarness) input(guests int) CreateInput {
	id := uuid.New().String()
	return CreateInput{
		RestaurantID: h.restaurantID, Name: "Гость", Phone: "+7701" + id[:7],
		Guests: guests, StartsAt: h.startsAt, Source: domain.SourceAdmin,
	}
}

func (h *overrideHarness) overbook(guests int) CreateInput {
	in := h.input(guests)
	in.Overbook = true
	return in
}

func (h *overrideHarness) peakTaken(t *testing.T) (taken, limit int) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT coalesce(max(seats_taken),0), coalesce(max(seats_limit),0)
		 FROM restaurant_capacity_buckets WHERE restaurant_id=$1`, h.restaurantID).Scan(&taken, &limit)
	if err != nil {
		t.Fatalf("read buckets: %v", err)
	}
	return taken, limit
}

func (h *overrideHarness) overrideRows(t *testing.T) []domain.BookingCapacityOverride {
	t.Helper()
	rows, err := h.capacity.ListOverrides(context.Background(), h.restaurantID,
		h.startsAt.Add(-24*time.Hour), h.startsAt.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	return rows
}

// The whole feature end to end on real Postgres: who may override, what it does
// to the ledger, and that the CHECK still refuses everybody else afterwards.
func TestOverridePostgresEndToEnd(t *testing.T) {
	h := newOverrideHarness(t, 10)
	ctx := context.Background()

	// 8 of 10 seats sold by an ordinary guest booking.
	if _, err := h.create.Create(ctx, h.guest, h.input(8)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	// A party of 4 does not fit, and the DATABASE is what says so.
	if _, err := h.create.Create(ctx, h.manager, h.input(4)); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("party of 4 into 2 free seats = %v, want ErrAlreadyExists", err)
	}
	// A guest cannot override, and neither can staff of another venue.
	if _, err := h.create.Create(ctx, h.guest, h.overbook(4)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("guest override = %v, want ErrForbidden", err)
	}
	if _, err := h.create.Create(ctx, h.stranger, h.overbook(4)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("outside manager override = %v, want ErrForbidden", err)
	}
	if rows := h.overrideRows(t); len(rows) != 0 {
		t.Fatalf("refused overrides wrote %d audit rows, want 0", len(rows))
	}

	// The venue's own manager seats them anyway.
	details, err := h.create.Create(ctx, h.manager, h.overbook(4))
	if err != nil {
		t.Fatalf("staff override: %v", err)
	}

	// The seats are COUNTED. Not sitting beside the ledger — inside it.
	var holdSeats int
	if err := h.pool.QueryRow(ctx,
		`SELECT coalesce(sum(seats),0) FROM booking_capacity_holds WHERE booking_id=$1 AND active`,
		details.Booking.ID).Scan(&holdSeats); err != nil {
		t.Fatalf("read holds: %v", err)
	}
	if holdSeats == 0 {
		t.Fatal("an overridden booking must still hold capacity")
	}
	if taken, limit := h.peakTaken(t); taken != 12 || limit != 12 {
		t.Fatalf("peak bucket = %d/%d, want 12/12 (counted, with no headroom left)", taken, limit)
	}

	// Exactly one audit row, answering "who put 12 people in a 10-seat room".
	rows := h.overrideRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(rows))
	}
	o := rows[0]
	if o.BookingID != details.Booking.ID || o.ActorUserID != h.manager.UserID ||
		o.ActorType != domain.ActorManager || o.Guests != 4 ||
		o.DeclaredSeats != 10 || o.SeatsOver != 2 || o.PeakSeatsTaken != 12 {
		t.Fatalf("audit row = %+v", o)
	}

	// And the guarantee is intact: the next ordinary guest is still refused, by
	// the CHECK, not by anything this feature added.
	if _, err := h.create.Create(ctx, h.guest, h.input(1)); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("booking after the override = %v, want ErrAlreadyExists", err)
	}
	if taken, _ := h.peakTaken(t); taken != 12 {
		t.Fatalf("seats_taken after the refused booking = %d, want 12", taken)
	}
}

// Reverting. Cancelling an overridden booking must give back exactly what it
// took, and the venue must NOT keep the raised limit: the next booking is
// measured against the declared capacity again.
func TestOverrideCancelReleasesAndIsNotInherited(t *testing.T) {
	h := newOverrideHarness(t, 10)
	ctx := context.Background()

	if _, err := h.create.Create(ctx, h.guest, h.input(8)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	over, err := h.create.Create(ctx, h.manager, h.overbook(4))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if taken, _ := h.peakTaken(t); taken != 12 {
		t.Fatalf("before the cancel seats_taken = %d, want 12", taken)
	}

	if err := h.bookings.UpdateStatus(ctx, over.Booking.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Exactly what it took, no more and no less.
	if taken, _ := h.peakTaken(t); taken != 8 {
		t.Fatalf("after the cancel seats_taken = %d, want 8", taken)
	}

	// The raised limit is not inherited: a party of 3 would fit under the
	// leftover 12 but not under the declared 10, and must be refused.
	if _, err := h.create.Create(ctx, h.guest, h.input(3)); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("party of 3 into a 10-seat room holding 8 = %v, want ErrAlreadyExists", err)
	}
	// While the two seats the venue really has are still sellable.
	if _, err := h.create.Create(ctx, h.guest, h.input(2)); err != nil {
		t.Fatalf("the two remaining real seats must still be bookable: %v", err)
	}
	if taken, limit := h.peakTaken(t); taken != 10 || limit != 10 {
		t.Fatalf("peak bucket = %d/%d, want 10/10 — the bucket is back under the declared capacity", taken, limit)
	}
	// The cancelled override is still on record: an audit trail does not
	// un-happen when the booking does.
	if rows := h.overrideRows(t); len(rows) != 1 {
		t.Fatalf("audit rows after the cancel = %d, want 1", len(rows))
	}
}

// The audit is append-only in the DATABASE, not by convention: a row that the
// application can rewrite settles no argument.
func TestOverrideAuditIsAppendOnly(t *testing.T) {
	h := newOverrideHarness(t, 10)
	ctx := context.Background()

	if _, err := h.create.Create(ctx, h.guest, h.input(8)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	if _, err := h.create.Create(ctx, h.manager, h.overbook(4)); err != nil {
		t.Fatalf("override: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE booking_capacity_overrides SET seats_over=0 WHERE restaurant_id=$1`, h.restaurantID); err == nil {
		t.Error("an audit row was rewritten")
	}
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM booking_capacity_overrides WHERE restaurant_id=$1`, h.restaurantID); err == nil {
		t.Error("an audit row was deleted")
	}
	if rows := h.overrideRows(t); len(rows) != 1 {
		t.Fatalf("audit rows = %d, want the original 1", len(rows))
	}
}

// The race that matters: an override and an ordinary booking hitting the same
// window at the same moment. Whoever wins, the room may never hold more than the
// declared capacity PLUS what was explicitly overridden and recorded — that sum
// is the definition of "what was allowed".
func TestOverrideConcurrentWithNormalCreate(t *testing.T) {
	h := newOverrideHarness(t, 10)
	ctx := context.Background()
	const declared = 10

	if _, err := h.create.Create(ctx, h.guest, h.input(8)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	var (
		wg   sync.WaitGroup
		gate = make(chan struct{})
		errs = make([]error, 2)
	)
	requests := []struct {
		actor Actor
		in    CreateInput
	}{
		{h.manager, h.overbook(4)}, // "we'll bring a chair"
		{h.guest, h.input(2)},      // the last two real seats
	}
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			_, errs[i] = h.create.Create(ctx, requests[i].actor, requests[i].in)
		}(i)
	}
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("request %d failed unexpectedly: %v", i, err)
		}
	}
	if errs[0] != nil && errs[1] != nil {
		t.Fatal("both requests lost; at least one had to be seatable")
	}

	allowed := declared
	for _, o := range h.overrideRows(t) {
		allowed += o.SeatsOver
	}
	taken, _ := h.peakTaken(t)
	if taken > allowed {
		t.Fatalf("room holds %d guests but only %d were allowed (declared %d + recorded overrides)",
			taken, allowed, declared)
	}
	// And the override, if it went through, is on record — never silent.
	if errs[0] == nil {
		if rows := h.overrideRows(t); len(rows) != 1 {
			t.Fatalf("the override committed but wrote %d audit rows, want 1", len(rows))
		}
	}
}
