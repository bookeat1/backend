package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedDish inserts one menu item for a venue. It writes straight to the table
// rather than going through the menu repository: this package must not depend
// on a sibling repository just to build a fixture, and the columns that matter
// to the search (name, name_i18n, is_available) are exactly the ones spelled
// out here.
func seedDish(t *testing.T, pool *pgxpool.Pool, restaurantID uuid.UUID, name string, available bool, nameI18n string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var i18n any
	if nameI18n != "" {
		i18n = nameI18n
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO menu_items (id, restaurant_id, name, name_i18n, price, is_available)
		 VALUES ($1, $2, $3, $4::jsonb, 2500, $5)`,
		id, restaurantID, name, i18n, available)
	if err != nil {
		t.Fatalf("seed dish %q: %v", name, err)
	}
	return id
}

// TestSearchFindsVenueByDishName is the whole point of the feature: a venue
// whose own name, description and cuisine say nothing about pasta is found by
// «паста» because it cooks it — and the result says WHICH dish matched, so the
// card can explain itself instead of looking like a bad result.
func TestSearchFindsVenueByDishName(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	venue := seedSearch(t, repo, "Ocean Blue", "seafood and a view", "Морская", domain.CityAlmaty)
	dish := seedDish(t, pool, venue, "Паста карбонара", true, "")
	// A decoy venue with a menu that has nothing to do with the query: it must
	// not be dragged in by the join.
	other := seedSearch(t, repo, "Grill House", "steaks", "Европейская", domain.CityAlmaty)
	seedDish(t, pool, other, "Рибай стейк", true, "")

	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != venue {
		t.Fatalf("dish search did not find exactly the pasta venue: total=%d ids=%v", total, ids(items))
	}
	if items[0].MatchedDish == nil {
		t.Fatalf("venue found through its menu carries no MatchedDish — the card cannot say why it matched")
	}
	if items[0].MatchedDish.ID != dish {
		t.Errorf("MatchedDish.ID = %v, want %v", items[0].MatchedDish.ID, dish)
	}
	if items[0].MatchedDish.Name != "Паста карбонара" {
		t.Errorf("MatchedDish.Name = %q, want %q", items[0].MatchedDish.Name, "Паста карбонара")
	}
}

// TestSearchIgnoresStopListedDish: a dish taken off the menu must not pull its
// venue into the result. A guest who comes for a pasta that the kitchen has
// stopped is a worse outcome than not finding the venue at all.
func TestSearchIgnoresStopListedDish(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	venue := seedSearch(t, repo, "Ocean Blue", "seafood and a view", "Морская", domain.CityAlmaty)
	dish := seedDish(t, pool, venue, "Паста карбонара", false, "")

	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("stop-listed dish still pulled its venue in: total=%d ids=%v", total, ids(items))
	}

	// Same row back on the menu: the venue reappears, no reindex, no cache.
	if _, err := pool.Exec(ctx, `UPDATE menu_items SET is_available = true WHERE id = $1`, dish); err != nil {
		t.Fatalf("return dish to the menu: %v", err)
	}
	items, total, err = repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста"})
	if err != nil {
		t.Fatalf("search after return: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != venue {
		t.Fatalf("dish back on the menu did not restore the hit: total=%d ids=%v", total, ids(items))
	}
}

// TestSearchByVenueTextStillWorks pins the pre-existing behaviour that the menu
// join must not disturb: name and description still match, and a venue found
// that way carries no MatchedDish (there is nothing to explain).
func TestSearchByVenueTextStillWorks(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	byName := seedSearch(t, repo, "Pasta House", "italian classics", "Итальянская", domain.CityAlmaty)
	byDesc := seedSearch(t, repo, "Roma", "fresh pasta made daily", "Итальянская", domain.CityAlmaty)
	// Neither venue has any menu at all: the LEFT JOIN must not drop them.
	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "pasta"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 {
		t.Fatalf("venue name/description search = %d hits, want 2 (ids=%v)", total, ids(items))
	}
	if !containsID(items, byName) || !containsID(items, byDesc) {
		t.Fatalf("expected both %v and %v, got %v", byName, byDesc, ids(items))
	}
	for _, it := range items {
		if it.MatchedDish != nil {
			t.Errorf("venue %v matched by its own text carries MatchedDish %q", it.ID, it.MatchedDish.Name)
		}
	}
}

// TestSearchVenueTextOutranksDish: "this place IS that" beats "this place cooks
// that". Both venues match «паста», the one named for it must come first.
func TestSearchVenueTextOutranksDish(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	named := seedSearch(t, repo, "Паста Бар", "italian classics", "Итальянская", domain.CityAlmaty)
	byDish := seedSearch(t, repo, "Ocean Blue", "seafood and a view", "Морская", domain.CityAlmaty)
	seedDish(t, pool, byDish, "Паста карбонара", true, "")

	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("expected both venues, got total=%d ids=%v", total, ids(items))
	}
	if items[0].ID != named || items[1].ID != byDish {
		t.Errorf("ranking: got %v, want [%v %v] (venue-text hit first)", ids(items), named, byDish)
	}
}

// TestSearchDishTypoTolerance: a typo in a DISH name is forgiven exactly the way
// a typo in a venue name is — same <% word-similarity operator, same threshold.
func TestSearchDishTypoTolerance(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	venue := seedSearch(t, repo, "Ocean Blue", "seafood and a view", "Морская", domain.CityAlmaty)
	seedDish(t, pool, venue, "Карбонара", true, "")

	items, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "карбонра"})
	if err != nil {
		t.Fatalf("typo search: %v", err)
	}
	if !containsID(items, venue) {
		t.Fatalf("typo'd dish name did not find the venue via trigram; got %v", ids(items))
	}
}

// TestSearchDishI18n: dish names are searched across ALL locales, the same rule
// the venue's own text follows — a guest reading the app in English types
// "carbonara" and finds the venue whose ru name is «Карбонара».
func TestSearchDishI18n(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	venue := seedSearch(t, repo, "Ocean Blue", "seafood and a view", "Морская", domain.CityAlmaty)
	seedDish(t, pool, venue, "Карбонара", true, `{"en":"Carbonara pasta","kk":"Карбонара"}`)

	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "carbonara"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || !containsID(items, venue) {
		t.Fatalf("translated dish name not searchable: total=%d ids=%v", total, ids(items))
	}
	if items[0].MatchedDish == nil || items[0].MatchedDish.NameI18n["en"] != "Carbonara pasta" {
		t.Errorf("MatchedDish did not carry its translations: %+v", items[0].MatchedDish)
	}
}

// TestSearchDishRespectsFilters: the menu join is ANDed with the ordinary
// filters, not ORed past them. Two venues cook the same dish; the city filter
// must still cut the result to one.
func TestSearchDishRespectsFilters(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	almaty := seedSearch(t, repo, "Ocean Blue", "seafood", "Морская", domain.CityAlmaty)
	seedDish(t, pool, almaty, "Паста карбонара", true, "")
	astana := seedSearch(t, repo, "Ocean Blue Astana", "seafood", "Морская", domain.CityAstana)
	seedDish(t, pool, astana, "Паста карбонара", true, "")

	city := domain.CityAlmaty
	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста", City: &city})
	if err != nil {
		t.Fatalf("filtered dish search: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != almaty {
		t.Fatalf("city filter did not narrow the dish search: total=%d ids=%v", total, ids(items))
	}

	// Cuisine filter (an OR-set over a different dimension) combined with the
	// dish query: the Almaty venue's cuisine is «Морская», so a «Итальянская»
	// filter must leave nothing, even though the dish matches.
	empty, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{
		Query: "паста", Cuisines: []string{"Итальянская"},
	})
	if err != nil {
		t.Fatalf("cuisine-filtered dish search: %v", err)
	}
	if total != 0 || len(empty) != 0 {
		t.Fatalf("cuisine filter ignored on a dish hit: total=%d ids=%v", total, ids(empty))
	}
}

// TestSearchDishNoDuplicateRows: a venue whose name AND several of whose dishes
// match must come back ONCE. The join is pre-aggregated per venue precisely so
// the page cannot be padded with duplicates of the same venue — and the count
// query must agree with the rows.
func TestSearchDishNoDuplicateRows(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	venue := seedSearch(t, repo, "Паста Бар", "italian classics", "Итальянская", domain.CityAlmaty)
	seedDish(t, pool, venue, "Паста карбонара", true, "")
	seedDish(t, pool, venue, "Паста болоньезе", true, "")
	seedDish(t, pool, venue, "Паста песто", true, "")

	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "паста"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != venue {
		t.Fatalf("venue with 3 matching dishes came back %d times (total=%d)", len(items), total)
	}
}
