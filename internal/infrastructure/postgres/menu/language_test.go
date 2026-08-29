package menu

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// A translation stored as a SEPARATE ROW (the shape part of the imported data
// has: a Kazakh copy of a dish that also exists in Russian) must never be
// listed next to its original — that is the same dish twice — and must never
// replace it either.
func TestListByRestaurantServesBaseRowsOnlyAndNeverDuplicates(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)

	ru, kk := "ru", "kk"
	base := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Бешбармак",
		NameI18n: domain.I18n{"ru": "Бешбармак", "kk": "Бешбармақ"},
		Price:    "3200.00", IsAvailable: true, Language: &ru,
	}
	noLabel := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Плов",
		Price: "2500.00", IsAvailable: true, // Language nil — also a base row
	}
	copyRow := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Бешбармақ",
		Price: "3200.00", IsAvailable: true, Language: &kk,
	}
	for _, m := range []*domain.MenuItem{base, noLabel, copyRow} {
		if err := repo.Create(ctx, m); err != nil {
			t.Fatalf("create %s: %v", m.Name, err)
		}
	}

	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 base rows, got %d", len(items))
	}
	got := map[uuid.UUID]bool{}
	for _, it := range items {
		if got[it.ID] {
			t.Fatalf("dish %s returned twice", it.ID)
		}
		got[it.ID] = true
	}
	if !got[base.ID] || !got[noLabel.ID] {
		t.Fatalf("base rows missing: %v", got)
	}
	if got[copyRow.ID] {
		t.Fatalf("the per-language copy row %s leaked into the listing", copyRow.ID)
	}
}

// The safety net: a venue whose menu was uploaded ENTIRELY in one non-Russian
// language has no base rows at all. Filtering it down to nothing would turn
// "untranslated" into "this venue has no menu", which is the bug we are fixing,
// only reversed — so those rows ARE served.
func TestListByRestaurantFallsBackWhenAVenueHasNoBaseRows(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	kk := "kk"
	for _, name := range []string{"Бешбармақ", "Қуырдақ"} {
		if err := repo.Create(ctx, &domain.MenuItem{
			ID: uuid.New(), RestaurantID: rid, Name: name,
			Price: "3200.00", IsAvailable: true, Language: &kk,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("a venue with only non-base rows must still get its menu, got %d", len(items))
	}
}

// The label is free text, so a spreadsheet import spelling it 'RU' must still
// read as a base row rather than as somebody's translation copy.
func TestListByRestaurantTreatsTheLabelCaseInsensitively(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	upper, kk := "RU", "kk"
	if err := repo.Create(ctx, &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Плов", Price: "2500.00",
		IsAvailable: true, Language: &upper,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Палау", Price: "2500.00",
		IsAvailable: true, Language: &kk,
	}); err != nil {
		t.Fatalf("create copy: %v", err)
	}

	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Name != "Плов" {
		t.Fatalf("want only the 'RU' base row, got %d: %+v", len(items), items)
	}
}
