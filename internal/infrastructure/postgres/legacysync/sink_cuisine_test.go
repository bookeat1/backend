package legacysync_test

import (
	"context"
	"testing"

	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestSyncDoesNotOverwriteCuisine is the guard on ADR-023.
//
// Cuisine is OURS since migration 0079: it is chosen in our panel and stored in
// restaurant_cuisines, and restaurants.cuisine_type is that set rendered as a
// string. The legacy system still owns name/address/city, and its rows still
// carry a free-text cuisine — the very «Кафе, европейская» values the
// dictionary exists to replace.
//
// So: a venue arriving for the FIRST time keeps the legacy string (better than
// an empty cuisine), and every subsequent sync run must leave whatever we have
// decided since completely alone. Without this the next sync silently reverts
// the whole migration, and nothing anywhere reports an error.
func TestSyncDoesNotOverwriteCuisine(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)

	src := newFakeSource()
	w := newWorker(src, pool)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// First insert: the legacy value lands, because we have nothing better yet.
	var got string
	if err := pool.QueryRow(ctx, `SELECT cuisine_type FROM restaurants WHERE id = $1`, rest1).Scan(&got); err != nil {
		t.Fatalf("read cuisine after first sync: %v", err)
	}
	if got != "italian" {
		t.Fatalf("cuisine_type after first sync = %q, want the legacy value %q", got, "italian")
	}

	// Now WE decide the venue's cuisine, the way usecase/cuisines does.
	if _, err := pool.Exec(ctx,
		`UPDATE restaurants SET cuisine_type = $2, cuisine_type_i18n = $3 WHERE id = $1`,
		rest1, "Итальянская, Европейская", []byte(`{"en":"Italian, European"}`)); err != nil {
		t.Fatalf("set our cuisine: %v", err)
	}

	// The legacy row changes — including its cuisine — and syncs again.
	src.restaurants[0].Name = "Rest One Renamed"
	src.restaurants[0].CuisineType = "italian, cafe"
	src.restaurants[0].UpdatedAt = t0(5)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	var name, cuisine string
	var i18n []byte
	if err := pool.QueryRow(ctx,
		`SELECT name, cuisine_type, cuisine_type_i18n FROM restaurants WHERE id = $1`,
		rest1).Scan(&name, &cuisine, &i18n); err != nil {
		t.Fatalf("read after second sync: %v", err)
	}
	// The columns legacy still owns DID update — otherwise this test would
	// also pass if the sync simply stopped working.
	if name != "Rest One Renamed" {
		t.Errorf("name = %q, want the legacy update to still apply", name)
	}
	if cuisine != "Итальянская, Европейская" {
		t.Errorf("cuisine_type = %q, want OUR value untouched by the sync", cuisine)
	}
	if string(i18n) != `{"en": "Italian, European"}` && string(i18n) != `{"en":"Italian, European"}` {
		t.Errorf("cuisine_type_i18n = %s, want OUR value untouched by the sync", i18n)
	}
}
