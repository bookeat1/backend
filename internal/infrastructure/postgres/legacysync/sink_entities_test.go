package legacysync_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedOurVenue inserts a venue the way THIS system owns it: created/edited in
// our admin panel, with a name the owner typed, and with the legacy id (ids are
// reused verbatim, which is the whole reason the old sync could overwrite it).
func seedOurVenue(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, name string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO restaurants (id, name, name_i18n, description, address, opening_hours, city, price_category)
VALUES ($1, $2::varchar, jsonb_build_object('ru', $2::varchar), 'Наше описание', 'Наш адрес', '11:00–23:00', 'Алматы', '₸₸')`,
		id, name)
	if err != nil {
		t.Fatalf("seed our venue: %v", err)
	}
}

// TestDefaultSyncWritesOnlyBookings is the guard on the narrowed sync.
//
// The old base is the web site's engine now; the mobile apps and the admin
// panel run on this one. So a pass must bring the bookings the web site takes
// and touch NOTHING else — the venue, its tables, its menu and its schedule are
// ours, and re-importing them is how an owner's rename silently reverted
// (complaint of 2026-08-27).
func TestDefaultSyncWritesOnlyBookings(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)

	// The venue exists HERE, renamed by its owner in the cabinet, while the
	// legacy row still carries the old wording.
	seedOurVenue(t, pool, rest1, "Тбилиси")

	src := newFakeSource()
	src.restaurants[0].Name = "THE ME'ET" // the stale legacy name
	src.restaurants[0].UpdatedAt = t0(9)  // and it "changed", so it is in the batch

	w := newDefaultWorker(src, pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// The bookings DID arrive — this is the one thing the sync is still for.
	if got := count(t, pool, "bookings"); got != 2 {
		t.Fatalf("bookings = %d, want the legacy bookings imported", got)
	}

	// ...and nothing else did.
	var name, description, address, hours string
	var nameI18n []byte
	if err := pool.QueryRow(ctx,
		`SELECT name, name_i18n, description, address, opening_hours FROM restaurants WHERE id = $1`, rest1).
		Scan(&name, &nameI18n, &description, &address, &hours); err != nil {
		t.Fatalf("read venue: %v", err)
	}
	if name != "Тбилиси" {
		t.Errorf("name = %q, want the cabinet's value untouched by the sync", name)
	}
	if string(nameI18n) != `{"ru": "Тбилиси"}` && string(nameI18n) != `{"ru":"Тбилиси"}` {
		t.Errorf("name_i18n = %s, want the cabinet's translation untouched", nameI18n)
	}
	if description != "Наше описание" || address != "Наш адрес" || hours != "11:00–23:00" {
		t.Errorf("profile text overwritten: description=%q address=%q hours=%q", description, address, hours)
	}
	for _, table := range []string{"restaurant_tables", "menu_categories", "menu_items",
		"restaurant_working_hours", "legacy_working_hours_import", "booking_tables"} {
		if got := count(t, pool, table); got != 0 {
			t.Errorf("%s = %d rows, want 0 — the entity is not in the allowlist", table, got)
		}
	}
	// A disabled entity must not advance a cursor either: nothing was read for
	// it, and a cursor that moved would silently skip rows if it were ever
	// enabled again.
	var cursors int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM legacy_sync_cursor WHERE entity <> 'bookings'`).Scan(&cursors); err != nil {
		t.Fatalf("count cursors: %v", err)
	}
	if cursors != 0 {
		t.Errorf("cursors for disabled entities = %d, want 0", cursors)
	}
}

// TestDefaultSyncParksBookingForUnknownVenue answers the question the narrowing
// creates: what happens to a booking whose venue is not in this database?
//
// It is PARKED — held, retried on every later tick, and the cursor does not
// move past it. It is not written with an invented parent (a stub venue would
// show up in the catalog), and it is not dropped (a real reservation nobody
// would ever see again). Nothing outside `bookings` is created.
func TestDefaultSyncParksBookingForUnknownVenue(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool) // no venue seeded on purpose

	w := newDefaultWorker(newFakeSource(), pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := count(t, pool, "bookings"); got != 0 {
		t.Errorf("bookings = %d, want the rows parked, not written", got)
	}
	if got := count(t, pool, "restaurants"); got != 0 {
		t.Errorf("restaurants = %d, want NO stub venue invented for the parent", got)
	}
	// The cursor must stay put, or the parked rows would be skipped forever.
	var advanced int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM legacy_sync_cursor WHERE entity = 'bookings'`).Scan(&advanced); err != nil {
		t.Fatalf("count booking cursor: %v", err)
	}
	if advanced != 0 {
		t.Errorf("bookings cursor rows = %d, want none — a parked row must be retried", advanced)
	}

	// And once the venue does exist here, the very next tick lands them.
	seedOurVenue(t, pool, rest1, "Тбилиси")
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := count(t, pool, "bookings"); got != 2 {
		t.Errorf("bookings after the venue appeared = %d, want 2", got)
	}
}

// TestSyncNeverTouchesUsers pins the other half of "only bookings and users":
// there is no users entity in this sync at all, so a guest's own data (phone,
// name, birthdate, language) cannot be reverted by a pass the way a venue's
// name was. A legacy booking made by a web account simply arrives with
// user_id = NULL and its name/phone, and the guest reclaims it by verifying
// that phone.
func TestSyncNeverTouchesUsers(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	seedOurVenue(t, pool, rest1, "Тбилиси")

	// The guest edits their profile in the app.
	if _, err := pool.Exec(ctx,
		`UPDATE users SET full_name = $2, phone = $3, preferred_language = $4 WHERE id = $1`,
		usr1, "Дамир", "+77011234567", "kk"); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// Every entity enabled — the widest a one-off import could ever be.
	w := newWorker(newFakeSource(), pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var name, phone, lang string
	if err := pool.QueryRow(ctx,
		`SELECT full_name, phone, preferred_language FROM users WHERE id = $1`, usr1).
		Scan(&name, &phone, &lang); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if name != "Дамир" || phone != "+77011234567" || lang != "kk" {
		t.Errorf("user data changed by the sync: name=%q phone=%q lang=%q", name, phone, lang)
	}
	if got := count(t, pool, "users"); got != 1 {
		t.Errorf("users = %d rows, want the sync to create none", got)
	}
	// The booking of a legacy user this DB does not know keeps no owner...
	var owner *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM bookings WHERE id = $1`, book2).Scan(&owner); err != nil {
		t.Fatalf("read book2: %v", err)
	}
	if owner != nil {
		t.Errorf("user_id = %v, want NULL for a user that does not exist here", owner)
	}
	// ...and the booking is still there to be claimed later, with its contacts.
	var bookingName, bookingPhone string
	if err := pool.QueryRow(ctx, `SELECT name, phone_normalized FROM bookings WHERE id = $1`, book2).
		Scan(&bookingName, &bookingPhone); err != nil {
		t.Fatalf("read book2 contacts: %v", err)
	}
	if bookingName == "" || bookingPhone == "" {
		t.Errorf("booking arrived without contacts: name=%q phone=%q", bookingName, bookingPhone)
	}
}

// TestOneOffImportStillPossible: the allowlist is a switch, not a deletion. An
// operator can still ask for a full import for one run.
func TestOneOffImportStillPossible(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)

	w := newWorker(newFakeSource(), pool) // every entity
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, table := range []string{"restaurants", "restaurant_tables", "menu_categories", "menu_items"} {
		if got := count(t, pool, table); got == 0 {
			t.Errorf("%s = 0 rows, want the explicit full import to still work", table)
		}
	}
	_ = time.Second
}
