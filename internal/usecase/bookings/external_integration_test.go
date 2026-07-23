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

// externalIntegrationTables are wiped before each external-hold test, children
// first. Restaurants/users are seeded fresh per harness and not truncated.
var externalIntegrationTables = []string{
	"booking_outbox", "booking_status_history", "booking_rate_log",
	"booking_items", "booking_tables", "external_reservations", "bookings",
}

// externalHarness wires the external-hold, create and availability usecases over
// the REAL Postgres repositories and the REAL sqltx.Manager, so it observes what
// the GiST exclusion constraint on booking_tables actually does across sources.
type externalHarness struct {
	pool     *pgxpool.Pool
	external ExternalReservationUseCase
	create   CreateUseCase

	avail        AvailabilityUseCase
	restaurantID uuid.UUID
	tableID      uuid.UUID
	staffUID     uuid.UUID
	staff        Actor
	loc          *time.Location
	day          time.Time // target local calendar day, 2 days ahead
}

// externalTestConfig uses a ZERO cleanup buffer so slot boundaries are exact:
// an external hold [18:00,20:00) and a booking window starting at 20:00 do not
// overlap, and the test can assert the half-open boundary directly.
func externalTestConfig() Config {
	c := testConfig()
	c.DefaultBuffer = 0
	return c
}

type fakePerms struct{ allow map[[2]uuid.UUID]bool }

func (f fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, _ domain.Permission) (bool, error) {
	return f.allow[[2]uuid.UUID{userID, restaurantID}], nil
}

func newExternalHarness(t *testing.T) *externalHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, externalIntegrationTables...)
	ctx := context.Background()

	rid, tid, staffUID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,'R','Алматы','₸',true)`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, is_active)
		 VALUES ($1,$2,'T1',4,true)`, tid, rid); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Staff')`,
		staffUID, staffUID.String()+"@example.com", "+7707"+staffUID.String()[:6]); err != nil {
		t.Fatalf("seed staff user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM restaurant_tables WHERE restaurant_id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM restaurant_working_hours WHERE restaurant_id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM restaurants WHERE id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, staffUID)
	})

	txm := sqltx.NewManager(pool)
	cfg := externalTestConfig()
	perms := fakePerms{allow: map[[2]uuid.UUID]bool{{staffUID, rid}: true}}

	create := NewCreateUseCase(
		bookingrepo.New(pool), bookingrepo.NewTables(pool), bookingrepo.NewItems(pool),
		bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool), newFakeManagers(), txm, cfg,
	)
	external := NewExternalReservationUseCase(
		bookingrepo.NewExternalReservations(pool), restrepo.New(pool),
		restrepo.NewRelated(pool), perms, txm,
	)
	avail := NewAvailabilityUseCase(bookingrepo.NewTables(pool), restrepo.New(pool), restrepo.NewRelated(pool), cfg)

	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	d := time.Now().In(loc).AddDate(0, 0, 2)
	return &externalHarness{
		pool: pool, external: external, create: create, avail: avail,
		restaurantID: rid, tableID: tid, staffUID: staffUID,
		staff: Actor{UserID: staffUID, Role: domain.RoleRestaurant},
		loc:   loc,
		day:   time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc),
	}
}

// localStart returns the target day at hh:00 local time.
func (h *externalHarness) localStart(hour int) time.Time {
	return time.Date(h.day.Year(), h.day.Month(), h.day.Day(), hour, 0, 0, 0, h.loc)
}

// guestBooking is an anonymous (walk-in) booking: UserID nil so the test does
// not have to seed a users row per attempt. The distinct phone per call keeps
// each attempt clear of the per-phone anti-fraud limit.
func (h *externalHarness) guestBooking(hour int, phone string) CreateInput {
	return CreateInput{
		RestaurantID: h.restaurantID, Name: "Guest",
		Phone: phone, Email: "", Guests: 2,
		StartsAt: h.localStart(hour).UTC(), Source: domain.SourceApp,
	}
}

// A per-table external hold makes the slot unavailable in the calendar AND
// rejects a BookEat booking into it, while the half-open boundary right after
// the hold stays bookable — the whole point of the engine.
func TestExternalHoldBlocksAvailabilityAndBooking(t *testing.T) {
	h := newExternalHarness(t)
	ctx := context.Background()

	tid := h.tableID
	if _, err := h.external.Create(ctx, h.staff, h.restaurantID, ExternalHoldInput{
		TableID: &tid, StartsAt: h.localStart(18).UTC(), EndsAt: h.localStart(20).UTC(),
		Source: domain.ExtSourcePhone,
	}); err != nil {
		t.Fatalf("create hold: %v", err)
	}

	day, err := h.avail.Day(ctx, h.restaurantID, h.day.Format(DateLayout), 2)
	if err != nil {
		t.Fatalf("availability: %v", err)
	}
	if avail := slotAvailableAt(day, h.localStart(19)); avail {
		t.Fatalf("19:00 slot should be unavailable — the only table is held")
	}
	// Half-open boundary: a booking starting exactly when the hold ends (buffer 0)
	// must remain bookable.
	if avail := slotAvailableAt(day, h.localStart(20)); !avail {
		t.Fatalf("20:00 slot should be available — the hold ends at 20:00 (half-open)")
	}

	// A booking overlapping the hold is rejected (the only table is taken).
	if _, err := h.create.Create(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser},
		h.guestBooking(19, "+7 (700) 111-11-11")); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("booking into held slot = %v, want ErrAlreadyExists", err)
	}
	// A booking at the boundary succeeds.
	if _, err := h.create.Create(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser},
		h.guestBooking(20, "+7 (700) 222-22-22")); err != nil {
		t.Fatalf("booking at boundary = %v, want success", err)
	}
}

// Removing the hold frees the slot for both the calendar and new bookings.
func TestExternalHoldRemovalFreesSlot(t *testing.T) {
	h := newExternalHarness(t)
	ctx := context.Background()

	tid := h.tableID
	res, err := h.external.Create(ctx, h.staff, h.restaurantID, ExternalHoldInput{
		TableID: &tid, StartsAt: h.localStart(18).UTC(), EndsAt: h.localStart(20).UTC(),
		Source: domain.ExtSourceManual,
	})
	if err != nil {
		t.Fatalf("create hold: %v", err)
	}
	if _, err := h.create.Create(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser},
		h.guestBooking(19, "+7 (700) 333-33-33")); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("booking into held slot = %v, want ErrAlreadyExists", err)
	}

	if err := h.external.Delete(ctx, h.staff, h.restaurantID, res.ID); err != nil {
		t.Fatalf("delete hold: %v", err)
	}
	if _, err := h.create.Create(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser},
		h.guestBooking(19, "+7 (700) 444-44-44")); err != nil {
		t.Fatalf("booking after hold removed = %v, want success", err)
	}
}

// A whole-venue block (table_id nil) occupies every active table.
func TestExternalWholeVenueBlock(t *testing.T) {
	h := newExternalHarness(t)
	ctx := context.Background()

	if _, err := h.external.Create(ctx, h.staff, h.restaurantID, ExternalHoldInput{
		StartsAt: h.localStart(18).UTC(), EndsAt: h.localStart(20).UTC(),
		Source: domain.ExtSourceManual,
	}); err != nil {
		t.Fatalf("create whole-venue block: %v", err)
	}
	if _, err := h.create.Create(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser},
		h.guestBooking(19, "+7 (700) 555-55-55")); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("booking into whole-venue block = %v, want ErrAlreadyExists", err)
	}
}

// A non-staff actor cannot record a hold.
func TestExternalHoldRequiresPermission(t *testing.T) {
	h := newExternalHarness(t)
	tid := h.tableID
	_, err := h.external.Create(context.Background(),
		Actor{UserID: uuid.New(), Role: domain.RoleUser}, h.restaurantID, ExternalHoldInput{
			TableID: &tid, StartsAt: h.localStart(18).UTC(), EndsAt: h.localStart(20).UTC(),
		})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("guest creating a hold = %v, want ErrForbidden", err)
	}
}

// Two concurrent bookings for the single remaining table: exactly one wins, the
// other loses the GiST race with ErrAlreadyExists — never a double-book.
func TestConcurrentBookingsSameTable(t *testing.T) {
	h := newExternalHarness(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	phones := []string{"+7 (700) 611-11-11", "+7 (700) 622-22-22"}
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = h.create.Create(ctx,
				Actor{UserID: uuid.New(), Role: domain.RoleUser}, h.guestBooking(19, phones[i]))
		}(i)
	}
	close(start)
	wg.Wait()

	assertExactlyOneWinner(t, errs)
}

// Two concurrent per-table holds on the same table and window: exactly one wins.
func TestConcurrentExternalHoldsSameTable(t *testing.T) {
	h := newExternalHarness(t)
	ctx := context.Background()
	tid := h.tableID

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = h.external.Create(ctx, h.staff, h.restaurantID, ExternalHoldInput{
				TableID: &tid, StartsAt: h.localStart(18).UTC(), EndsAt: h.localStart(20).UTC(),
				Source: domain.ExtSourceManual,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	assertExactlyOneWinner(t, errs)
}

func assertExactlyOneWinner(t *testing.T, errs []error) {
	t.Helper()
	winners, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrAlreadyExists):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d, want exactly 1 each", winners, conflicts)
	}
}

// slotAvailableAt reports the Available flag of the slot starting at the given
// local time (compared as an instant), or fails the lookup by returning false.
func slotAvailableAt(day *DayAvailability, start time.Time) bool {
	for _, s := range day.Slots {
		if s.StartsAt.Equal(start) {
			return s.Available
		}
	}
	return false
}
