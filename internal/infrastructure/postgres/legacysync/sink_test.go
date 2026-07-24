package legacysync_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	legacysink "backend-core/internal/infrastructure/postgres/legacysync"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/logger"
	uc "backend-core/internal/usecase/legacysync"
)

// fakeSource is an in-memory, old-shaped Source. It honours the same
// (updated_at, id) cursor contract the real read adapter does, so the worker's
// incremental behaviour is exercised end to end against a real new-DB sink.
type fakeSource struct {
	restaurants []uc.Restaurant
	tables      []uc.Table
	categories  []uc.MenuCategory
	items       []uc.MenuItem
	bookings    []uc.LegacyBooking
	btables     []uc.LegacyBookingTable
}

func after(a, b uc.Cursor) bool {
	if a.UpdatedAt.After(b.UpdatedAt) {
		return true
	}
	if a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.ID.String() > b.ID.String()
	}
	return false
}

func page[T any](rows []T, cur uc.Cursor, limit int, key func(T) uc.Cursor) []T {
	var out []T
	for _, r := range rows {
		if after(key(r), cur) {
			out = append(out, r)
		}
	}
	// naive selection sort by cursor asc — fine for a handful of test rows
	for i := 0; i < len(out); i++ {
		min := i
		for j := i + 1; j < len(out); j++ {
			if after(key(out[min]), key(out[j])) {
				min = j
			}
		}
		out[i], out[min] = out[min], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeSource) Restaurants(_ context.Context, c uc.Cursor, n int) ([]uc.Restaurant, error) {
	return page(f.restaurants, c, n, uc.Restaurant.Cursor), nil
}
func (f *fakeSource) Tables(_ context.Context, c uc.Cursor, n int) ([]uc.Table, error) {
	return page(f.tables, c, n, uc.Table.Cursor), nil
}
func (f *fakeSource) MenuCategories(_ context.Context, c uc.Cursor, n int) ([]uc.MenuCategory, error) {
	return page(f.categories, c, n, uc.MenuCategory.Cursor), nil
}
func (f *fakeSource) MenuItems(_ context.Context, c uc.Cursor, n int) ([]uc.MenuItem, error) {
	return page(f.items, c, n, uc.MenuItem.Cursor), nil
}
func (f *fakeSource) Bookings(_ context.Context, c uc.Cursor, n int) ([]uc.LegacyBooking, error) {
	return page(f.bookings, c, n, uc.LegacyBooking.Cursor), nil
}
func (f *fakeSource) BookingTables(_ context.Context, c uc.Cursor, n int) ([]uc.LegacyBookingTable, error) {
	// The real adapter filters booking_tables by updated_at only (`>=`), leaving
	// the id-precise skip to the worker; mirror that here.
	var out []uc.LegacyBookingTable
	for _, r := range f.btables {
		if !r.UpdatedAt.Before(c.UpdatedAt) {
			out = append(out, r)
		}
	}
	return out, nil
}

var (
	rest1 = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	rest2 = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tbl1  = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	cat1  = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	item1 = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	usr1  = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	book1 = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	book2 = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	book3 = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")
	book4 = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000004")
	bt1   = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")
	ghost = uuid.MustParse("99999999-9999-9999-9999-999999999999")
)

func t0(sec int) time.Time {
	return time.Date(2026, 1, 1, 12, 0, sec, 0, time.UTC)
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		restaurants: []uc.Restaurant{{
			ID: rest1, Name: "Rest One", Description: "d", CuisineType: "italian",
			Address: "addr", OpeningHours: "10-22", City: "Алматы", PriceCategory: "₸₸",
			Email: "r@x.kz", Phone: "+7700", IsActive: true, HiddenFromHome: false,
			CreatedAt: t0(1), UpdatedAt: t0(1),
		}},
		tables: []uc.Table{{
			ID: tbl1, RestaurantID: rest1, Name: "T1", Capacity: 4, IsActive: true,
			CreatedAt: t0(1), UpdatedAt: t0(1),
		}},
		categories: []uc.MenuCategory{{
			ID: cat1, Name: "Mains", DisplayOrder: 1, CreatedAt: t0(1), UpdatedAt: t0(1),
		}},
		items: []uc.MenuItem{{
			ID: item1, RestaurantID: rest1, Name: "Plov", Description: "rice", Price: 2500,
			IsAvailable: true, CreatedAt: t0(1), UpdatedAt: t0(1),
		}},
		bookings: []uc.LegacyBooking{
			{ // guest booking linked to an existing new user
				ID: book1, UserID: &usr1, RestaurantID: rest1, Name: "Guest A",
				Phone: "8 701 123 45 67", Email: "A@X.KZ", Guests: 2, BookingDate: t0(10),
				Status: "confirmed", CreatedAt: t0(2), UpdatedAt: t0(2),
			},
			{ // user_id points at a user NOT in the new DB -> must be nulled, not parked
				ID: book2, UserID: &ghost, RestaurantID: rest1, Name: "Guest B",
				Phone: "+7 (701) 000-11-22", Email: "b@x.kz", Guests: 3, BookingDate: t0(11),
				Status: "cancelled", CreatedAt: t0(3), UpdatedAt: t0(3),
			},
		},
		btables: []uc.LegacyBookingTable{
			{ // explicit join row, active booking -> active hold with a slot
				ID: bt1, BookingID: book1, TableID: tbl1, BookingDate: t0(10),
				Status: "confirmed", CreatedAt: t0(2), UpdatedAt: t0(2),
			},
		},
	}
}

func newWorker(src uc.Source, pool *pgxpool.Pool) *uc.Worker {
	log := logger.New("error", "text")
	return uc.NewWorker(src, legacysink.NewSink(pool), uc.Config{
		TickInterval: time.Hour, BatchSize: 100, DefaultDuration: 90 * time.Minute,
	}, log)
}

func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	testdb.Truncate(t, pool, "booking_tables", "bookings", "menu_items",
		"menu_categories", "restaurant_tables", "restaurants", "users", "legacy_sync_cursor")
	if _, err := pool.Exec(context.Background(), `INSERT INTO users (id) VALUES ($1)`, usr1); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSyncFullThenIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	src := newFakeSource()
	w := newWorker(src, pool)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	if got := count(t, pool, "restaurants"); got != 1 {
		t.Fatalf("restaurants=%d want 1", got)
	}
	if got := count(t, pool, "restaurant_tables"); got != 1 {
		t.Fatalf("tables=%d want 1", got)
	}
	if got := count(t, pool, "menu_categories"); got != 1 {
		t.Fatalf("menu_categories=%d want 1", got)
	}
	if got := count(t, pool, "menu_items"); got != 1 {
		t.Fatalf("menu_items=%d want 1", got)
	}
	if got := count(t, pool, "bookings"); got != 2 {
		t.Fatalf("bookings=%d want 2", got)
	}
	if got := count(t, pool, "booking_tables"); got != 1 {
		t.Fatalf("booking_tables=%d want 1", got)
	}

	// status + phone mapping, and email lower-casing
	var status, phoneNorm, email, source string
	var userID *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, phone_normalized, email, source, user_id FROM bookings WHERE id=$1`, book1).
		Scan(&status, &phoneNorm, &email, &source, &userID); err != nil {
		t.Fatalf("read book1: %v", err)
	}
	if status != "confirmed" {
		t.Errorf("status=%q want confirmed", status)
	}
	if phoneNorm != "+77011234567" {
		t.Errorf("phone_normalized=%q want +77011234567", phoneNorm)
	}
	if email != "a@x.kz" {
		t.Errorf("email=%q want lower-cased", email)
	}
	if source != "app" {
		t.Errorf("source=%q want app", source)
	}
	if userID == nil || *userID != usr1 {
		t.Errorf("user_id=%v want %v (existing user preserved)", userID, usr1)
	}

	// ends_at derived = starts_at + 90m
	var starts, ends time.Time
	if err := pool.QueryRow(ctx, `SELECT starts_at, ends_at FROM bookings WHERE id=$1`, book1).
		Scan(&starts, &ends); err != nil {
		t.Fatalf("read times: %v", err)
	}
	if ends.Sub(starts) != 90*time.Minute {
		t.Errorf("ends-starts=%v want 90m", ends.Sub(starts))
	}

	// user_id guard: book2's user does not exist in new users -> NULL, not parked
	var uid2 *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM bookings WHERE id=$1`, book2).Scan(&uid2); err != nil {
		t.Fatalf("read book2 user: %v", err)
	}
	if uid2 != nil {
		t.Errorf("book2 user_id=%v want NULL (ghost user)", uid2)
	}

	// booking_tables: active hold, slot present
	var active bool
	var lower time.Time
	if err := pool.QueryRow(ctx, `SELECT active, lower(slot) FROM booking_tables WHERE id=$1`, bt1).
		Scan(&active, &lower); err != nil {
		t.Fatalf("read bt1: %v", err)
	}
	if !active {
		t.Errorf("bt1 active=false want true (confirmed booking)")
	}

	// idempotent re-run: no new rows, no dupes
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := count(t, pool, "bookings"); got != 2 {
		t.Fatalf("after re-run bookings=%d want 2 (no dupes)", got)
	}
	if got := count(t, pool, "booking_tables"); got != 1 {
		t.Fatalf("after re-run booking_tables=%d want 1 (no dupes)", got)
	}
}

func TestSyncIncrementalCursor(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	src := newFakeSource()
	w := newWorker(src, pool)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := count(t, pool, "bookings"); got != 2 {
		t.Fatalf("bookings=%d want 2", got)
	}

	// A brand-new booking appears in the source with a later updated_at.
	src.bookings = append(src.bookings, uc.LegacyBooking{
		ID: book3, RestaurantID: rest1, Name: "Guest C", Phone: "87019998877",
		Email: "c@x.kz", Guests: 4, BookingDate: t0(12), Status: "pending",
		CreatedAt: t0(20), UpdatedAt: t0(20),
	})
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := count(t, pool, "bookings"); got != 3 {
		t.Fatalf("bookings=%d want 3 after incremental add", got)
	}
	// cursor for bookings advanced to book3
	var lastID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT last_synced_id FROM legacy_sync_cursor WHERE entity='bookings'`).Scan(&lastID); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if lastID != book3 {
		t.Errorf("bookings cursor id=%v want %v", lastID, book3)
	}
}

func TestSyncParksBookingUntilParentSynced(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	src := newFakeSource()
	// A booking for rest2, which is NOT in the source yet. It must park (its
	// restaurant FK cannot resolve) and NOT advance the bookings cursor past it.
	src.bookings = append(src.bookings, uc.LegacyBooking{
		ID: book4, RestaurantID: rest2, Name: "Guest D", Phone: "87010001111",
		Email: "d@x.kz", Guests: 2, BookingDate: t0(13), Status: "confirmed",
		CreatedAt: t0(30), UpdatedAt: t0(30),
	})
	w := newWorker(src, pool)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if count(t, pool, "bookings") != 2 {
		t.Fatalf("book4 should have parked; got %d bookings", count(t, pool, "bookings"))
	}
	if _, err := pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("pool broken: %v", err)
	}

	// rest2 now appears in the source. Next tick syncs it first, then book4
	// resolves (the bookings cursor never advanced past the parked row).
	src.restaurants = append(src.restaurants, uc.Restaurant{
		ID: rest2, Name: "Rest Two", Description: "d", CuisineType: "kz", Address: "a",
		OpeningHours: "9-21", City: "Астана", PriceCategory: "₸", Email: "r2@x.kz",
		Phone: "+7701", IsActive: true, CreatedAt: t0(25), UpdatedAt: t0(25),
	})
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := count(t, pool, "bookings"); got != 3 {
		t.Fatalf("bookings=%d want 3 after parent synced", got)
	}
	var rid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT restaurant_id FROM bookings WHERE id=$1`, book4).Scan(&rid); err != nil {
		t.Fatalf("book4 not written after unpark: %v", err)
	}
	if rid != rest2 {
		t.Errorf("book4 restaurant=%v want %v", rid, rest2)
	}
}

func TestSyncBookingTableSynthesizedFromTableID(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	src := newFakeSource()
	// Emulate the real adapter's UNION arm for bookings.table_id: a synthesized
	// row (ID == uuid.Nil) that the worker turns into a deterministic id.
	src.btables = append(src.btables, uc.LegacyBookingTable{
		ID: uuid.Nil, BookingID: book1, TableID: tbl1, BookingDate: t0(10),
		Status: "confirmed", CreatedAt: t0(2), UpdatedAt: t0(2),
	})
	w := newWorker(src, pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// bt1 (explicit) + one synthesized row, both for (book1, tbl1) but different
	// ids. They share the same table+slot, so the second must be Skipped by the
	// exclusion constraint, not error the tick. Exactly one active hold survives.
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_tables WHERE table_id=$1 AND active`, tbl1).Scan(&active); err != nil {
		t.Fatalf("count active holds: %v", err)
	}
	if active != 1 {
		t.Fatalf("active holds on tbl1=%d want 1 (exclusion constraint holds)", active)
	}
}
