package legacysync_test

import (
	"context"
	"testing"

	"backend-core/internal/infrastructure/postgres/testdb"
	uc "backend-core/internal/usecase/legacysync"
)

// TestSyncDoesNotOverwriteCity is the guard on the city half of ADR-023.
//
// The city is chosen in OUR panel now. The legacy row still carries whatever
// city was typed into the old admin, and the legacy sync still owns the
// contact/marketing columns — so the update must land on those and step over
// `city`.
//
// The "and other columns still updated" half is not decoration: without it the
// test would also pass if the sync silently stopped writing restaurants at all,
// which is exactly the trap that had to be dodged for cuisine.
func TestSyncDoesNotOverwriteCity(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)

	src := newFakeSource()
	w := newWorker(src, pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	var got string
	if err := pool.QueryRow(ctx, `SELECT city FROM restaurants WHERE id = $1`, rest1).Scan(&got); err != nil {
		t.Fatalf("read city after first sync: %v", err)
	}
	if got != "Алматы" {
		t.Fatalf("city after first sync = %q, want the legacy value %q", got, "Алматы")
	}

	// The venue moves, and the move is recorded where it is now recorded: in
	// our panel (usecase/restaurants validates against domain.Cities()).
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET city = $2 WHERE id = $1`, rest1, "Астана"); err != nil {
		t.Fatalf("set our city: %v", err)
	}

	// The legacy row changes — phone, price category and its own stale city —
	// and syncs. (Name and address used to be the canary here; they are ours
	// since 2026-08-27, see TestSyncDoesNotOverwriteProfileText.)
	src.restaurants[0].Phone = "+7702"
	src.restaurants[0].PriceCategory = "₸₸₸"
	src.restaurants[0].City = "Алматы"
	src.restaurants[0].UpdatedAt = t0(5)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	var phone, priceCategory, city string
	if err := pool.QueryRow(ctx,
		`SELECT phone, price_category, city FROM restaurants WHERE id = $1`, rest1).
		Scan(&phone, &priceCategory, &city); err != nil {
		t.Fatalf("read after second sync: %v", err)
	}
	// The columns legacy still owns DID update — otherwise this test would
	// also pass if the sync simply stopped working.
	if phone != "+7702" {
		t.Errorf("phone = %q, want the legacy update to still apply", phone)
	}
	if priceCategory != "₸₸₸" {
		t.Errorf("price_category = %q, want the legacy update to still apply", priceCategory)
	}
	if city != "Астана" {
		t.Errorf("city = %q, want OUR value untouched by the sync", city)
	}
}

// TestSyncWritesCityOnInsert pins the other half: a venue seen for the first
// time has no city of ours, and `city` is NOT NULL (migration 0002), so the
// legacy value must still be written on INSERT.
func TestSyncWritesCityOnInsert(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)

	src := newFakeSource()
	src.restaurants = append(src.restaurants, uc.Restaurant{
		ID: rest2, Name: "Rest Two", Description: "d", CuisineType: "kz", Address: "a",
		OpeningHours: "9-21", City: "Астана", PriceCategory: "₸", Email: "r2@x.kz",
		Phone: "+7701", IsActive: true, CreatedAt: t0(25), UpdatedAt: t0(25),
	})
	w := newWorker(src, pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var city string
	if err := pool.QueryRow(ctx, `SELECT city FROM restaurants WHERE id = $1`, rest2).Scan(&city); err != nil {
		t.Fatalf("read city of the newly inserted venue: %v", err)
	}
	if city != "Астана" {
		t.Errorf("city of a first-time venue = %q, want the legacy value %q", city, "Астана")
	}
}
