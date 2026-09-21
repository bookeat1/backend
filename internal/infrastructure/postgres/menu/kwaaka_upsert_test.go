package menu

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestUpsertFromKwaaka_MatchesOnRestaurantAndKwaakaProductID exercises
// migration 0114's whole point: a re-sync of the SAME product must UPDATE the
// existing row (keeping its id, and therefore its FKs — tags, top-pick slot,
// is_featured) rather than inserting a duplicate.
func TestUpsertFromKwaaka_MatchesOnRestaurantAndKwaakaProductID(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id) VALUES ($1,'R','Алматы','₸','kw-r1')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)

	kwID := "kw-prod-1"
	first := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Наггетсы", Price: "2890.00",
		IsAvailable: true, Category: ptr("Основное"), KwaakaProductID: &kwID,
	}
	if err := repo.UpsertFromKwaaka(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	firstID := first.ID

	// Mark it as a venue editorial pick — this must survive the next sync.
	if err := repo.SetFeatured(ctx, rid, firstID, true); err != nil {
		t.Fatalf("set featured: %v", err)
	}

	// Re-sync: same restaurant + kwaaka product id, different price/name — a
	// NEW MenuItem value with a fresh (throwaway) ID, exactly like a second
	// worker Tick would build.
	second := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Наггетсы 9 шт", Price: "3100.00",
		IsAvailable: false, Category: ptr("Основное"), KwaakaProductID: &kwID,
	}
	if err := repo.UpsertFromKwaaka(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if second.ID != firstID {
		t.Fatalf("upsert must keep the existing row's id: first=%s second=%s", firstID, second.ID)
	}

	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d menu items, want exactly 1 (no duplicate row)", len(items))
	}
	got := items[0]
	if got.Name != "Наггетсы 9 шт" || got.Price != "3100.00" || got.IsAvailable {
		t.Errorf("row not updated as expected: %+v", got)
	}
	if !got.IsFeatured {
		t.Error("is_featured must survive a Kwaaka re-sync (it is not a column UpsertFromKwaaka writes)")
	}
}

// TestMarkUnavailableExceptKwaakaIDs_StopListsWhatFellOut asserts the
// "removed from Kwaaka's menu entirely" case: a dish whose id is not in the
// latest pass' keep set goes dark, but a hand-entered dish (no
// kwaaka_product_id) is never touched, and an already-unavailable Kwaaka dish
// is not double-counted.
func TestMarkUnavailableExceptKwaakaIDs_StopListsWhatFellOut(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id) VALUES ($1,'R','Алматы','₸','kw-r1')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)

	kept := ptr("kw-kept")
	dropped := ptr("kw-dropped")
	alreadyOff := ptr("kw-already-off")
	for _, m := range []*domain.MenuItem{
		{ID: uuid.New(), RestaurantID: rid, Name: "Kept", Price: "1.00", IsAvailable: true, KwaakaProductID: kept},
		{ID: uuid.New(), RestaurantID: rid, Name: "Dropped", Price: "1.00", IsAvailable: true, KwaakaProductID: dropped},
		{ID: uuid.New(), RestaurantID: rid, Name: "AlreadyOff", Price: "1.00", IsAvailable: false, KwaakaProductID: alreadyOff},
	} {
		if err := repo.UpsertFromKwaaka(ctx, m); err != nil {
			t.Fatalf("seed upsert: %v", err)
		}
	}
	// A hand-entered dish (no Kwaaka binding at all) must never be touched.
	manual := &domain.MenuItem{ID: uuid.New(), RestaurantID: rid, Name: "Manual", Price: "1.00", IsAvailable: true}
	if err := repo.Create(ctx, manual); err != nil {
		t.Fatalf("seed manual: %v", err)
	}

	changed, err := repo.MarkUnavailableExceptKwaakaIDs(ctx, rid, []string{*kept})
	if err != nil {
		t.Fatalf("mark unavailable: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (only 'Dropped' flips)", changed)
	}

	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]domain.MenuItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if !byName["Kept"].IsAvailable {
		t.Error("Kept must stay available")
	}
	if byName["Dropped"].IsAvailable {
		t.Error("Dropped must be stop-listed")
	}
	if byName["AlreadyOff"].IsAvailable {
		t.Error("AlreadyOff must stay unavailable")
	}
	if !byName["Manual"].IsAvailable {
		t.Error("a hand-entered dish must never be touched by the Kwaaka stop-list pass")
	}
}
