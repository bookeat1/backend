package menu

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	menuuc "backend-core/internal/usecase/menu"
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

// THE POINT OF THE WHOLE FIX, END TO END: a dish created the way the cabinet
// creates it must be visible to the guest — in every language.
//
// This test wires the REAL usecase to the REAL repository on a live database on
// purpose. The visibility rule lives in SQL and the write rule lives in the
// usecase; a fake on either side would let them drift apart, and the drift is
// exactly the bug (a dish labelled with a non-base language at a venue that has
// base rows would be filtered out of every language at once, while looking
// perfectly normal in the cabinet).
func TestADishCreatedThroughTheCabinetIsVisibleToTheGuest(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	facade := menuuc.NewFacade(repo, NewCategories(pool), sqltx.NewManager(pool))

	// The venue already has a menu — this is what made the venue-level rule
	// dangerous: with base rows present, a non-base new dish disappears.
	ru := "ru"
	if err := repo.Create(ctx, &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Бешбармак", Price: "3200.00",
		IsAvailable: true, Language: &ru,
	}); err != nil {
		t.Fatalf("seed existing dish: %v", err)
	}

	name, price := "Плов", "2500.00"
	cases := []struct {
		name string
		lang *string
	}{
		{"no language at all", nil},
		{"the base language", ptr("ru")},
		{"the base language in another case", ptr("RU")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := facade.Create(ctx, rid, menuuc.ItemInput{
				Name: &name, Price: &price, Language: tc.lang,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			items, err := facade.ListByRestaurant(ctx, rid)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var found bool
			for _, it := range items {
				if it.ID == created.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("dish %s created through the cabinet is invisible to the guest (listing returned %d dishes)",
					created.ID, len(items))
			}
			if _, err := pool.Exec(ctx, `DELETE FROM menu_items WHERE id=$1`, created.ID); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		})
	}

	// The hostile input, stated as the invariant rather than as an
	// implementation detail: a cabinet write may be REFUSED, but it must never
	// quietly succeed into a dish no guest can see. This is what fails if the
	// write-side guard is ever removed while the venue-level visibility rule
	// stays.
	for _, lang := range []string{"en", "kk"} {
		t.Run("a non-base language never yields a saved-but-invisible dish ("+lang+")", func(t *testing.T) {
			l := lang
			created, err := facade.Create(ctx, rid, menuuc.ItemInput{Name: &name, Price: &price, Language: &l})
			if err != nil {
				return // refused up front — nothing was stored, nothing to hide
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(ctx, `DELETE FROM menu_items WHERE id=$1`, created.ID)
			})
			items, err := facade.ListByRestaurant(ctx, rid)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			for _, it := range items {
				if it.ID == created.ID {
					return
				}
			}
			t.Fatalf("dish %s was saved with language %q and is invisible to the guest (listing returned %d dishes)",
				created.ID, lang, len(items))
		})
	}
}

// The other half of the same guarantee: a write that WOULD produce an invisible
// dish is refused loudly, and nothing is stored. Rewriting the label to 'ru'
// silently is not an option — the text the editor typed is not Russian.
func TestTheCabinetCannotCreateADishTheGuestWouldNeverSee(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	facade := menuuc.NewFacade(New(pool), NewCategories(pool), sqltx.NewManager(pool))

	name, price := "Palau", "2500.00"
	for _, lang := range []string{"en", "kk", "kz"} {
		_, err := facade.Create(ctx, rid, menuuc.ItemInput{Name: &name, Price: &price, Language: &lang})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("language %q: want ErrValidation, got %v", lang, err)
		}
		code, ok := domain.CodeOf(err)
		if !ok || code != domain.CodeMenuItemLanguageNotBase {
			t.Fatalf("language %q: want code %q, got %q (present=%v)", lang, domain.CodeMenuItemLanguageNotBase, code, ok)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM menu_items WHERE restaurant_id=$1`, rid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused write left %d rows behind", n)
	}
}
