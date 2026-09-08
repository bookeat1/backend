package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedSearch inserts a restaurant with the given name/description/cuisine and
// returns its id. City/price default to Almaty/mid unless a venue-specific
// value matters to the assertion.
func seedSearch(t *testing.T, repo *Repository, name, desc, cuisine string, city domain.City) uuid.UUID {
	t.Helper()
	id := uuid.New()
	m := &domain.Restaurant{
		ID: id, Name: name, Description: desc, CuisineType: cuisine,
		City: city, PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(context.Background(), m); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
	return id
}

func TestSearchRankingAndTypoTolerance(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	// Both venues match the two-term query "Sushi Master" (plainto_tsquery ANDs
	// the terms). exact carries both terms twice (higher term frequency ->
	// higher ts_rank); decoy carries each once. The Pasta venue matches neither.
	exact := seedSearch(t, repo, "Sushi Master", "The Sushi Master signature omakase", "Японская", domain.CityAlmaty)
	decoy := seedSearch(t, repo, "Downtown Grill", "our master chef also rolls fresh sushi", "Европейская", domain.CityAlmaty)
	_ = seedSearch(t, repo, "Pasta House", "Italian pasta and wine", "Итальянская", domain.CityAlmaty)

	// Exact/FTS match: the venue named for the query, with both terms repeated,
	// must rank above the venue that only mentions each term once.
	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "Sushi Master"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total < 2 {
		t.Fatalf("expected >=2 hits for 'Sushi Master', got total=%d", total)
	}
	if items[0].ID != exact {
		t.Errorf("exact match did not rank first: got %v, want %v", items[0].ID, exact)
	}
	if items[1].ID != decoy {
		t.Errorf("second result = %v, want the lower-frequency decoy %v", items[1].ID, decoy)
	}

	// Typo tolerance via trigram: "Mastr" produces no matching FTS lexeme, so a
	// hit relies entirely on the pg_trgm word_similarity (<%) path at Postgres's
	// default word_similarity_threshold (0.6).
	typo, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{Query: "Sushi Mastr"})
	if err != nil {
		t.Fatalf("typo search: %v", err)
	}
	if !containsID(typo, exact) {
		t.Errorf("typo'd query did not find the venue via trigram; got %d results", len(typo))
	}
}

func TestSearchFiltersNarrow(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	almatyItalian := seedSearch(t, repo, "Roma Trattoria", "pizza and pasta", "Итальянская", domain.CityAlmaty)
	seedSearch(t, repo, "Astana Trattoria", "pizza and pasta", "Итальянская", domain.CityAstana)
	seedSearch(t, repo, "Almaty Sushi", "pizza-shaped sushi", "Японская", domain.CityAlmaty)

	// city + cuisine together must isolate the one Almaty Italian venue, even
	// though all three share the "pizza" text term.
	city := domain.CityAlmaty
	items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{
		Query: "pizza", City: &city, Cuisines: []string{"Итальянская"},
	})
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != almatyItalian {
		t.Fatalf("city+cuisine filter did not narrow to 1: total=%d ids=%v", total, ids(items))
	}

	// An empty query with a city filter degrades to a filtered browse (no text
	// constraint) and must still respect the filter.
	_, total, err = repo.Search(ctx, domain.RestaurantSearchFilter{City: &city})
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if total != 2 {
		t.Errorf("empty-query browse in Almaty = %d, want 2", total)
	}
}

func TestSearchPaginationStable(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	// Five venues sharing the same term with identical relevance — the id
	// tie-break is the only thing that keeps their order deterministic across
	// pages. Without it, equal-ranked rows could reshuffle and a row could be
	// skipped or duplicated between page 1 and page 2.
	for i := 0; i < 5; i++ {
		seedSearch(t, repo, "Coffee Point", "specialty coffee bar", "Кофейня", domain.CityAlmaty)
	}
	flt := func(page int) domain.RestaurantSearchFilter {
		return domain.RestaurantSearchFilter{Query: "coffee", Page: page, PerPage: 2}
	}

	var all []uuid.UUID
	for page := 1; page <= 3; page++ {
		items, total, err := repo.Search(ctx, flt(page))
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if total != 5 {
			t.Fatalf("page %d total = %d, want 5", page, total)
		}
		all = append(all, ids(items)...)
	}
	// Concatenating all three pages must yield 5 distinct ids in a strictly
	// increasing order (the id tie-break), proving no overlap/skip.
	if len(all) != 5 {
		t.Fatalf("paged through %d rows, want 5", len(all))
	}
	seen := map[uuid.UUID]bool{}
	for i, id := range all {
		if seen[id] {
			t.Fatalf("duplicate id across pages: %v", id)
		}
		seen[id] = true
		if i > 0 && all[i-1].String() >= id.String() {
			t.Errorf("id tie-break not strictly increasing at %d: %v then %v", i, all[i-1], id)
		}
	}

	// Re-fetching the same page must return the exact same rows in the same
	// order (stability under repeated reads).
	a, _, _ := repo.Search(ctx, flt(1))
	b, _, _ := repo.Search(ctx, flt(1))
	if len(a) != len(b) {
		t.Fatalf("page-1 size differs between reads")
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("page-1 order unstable at %d: %v vs %v", i, a[i].ID, b[i].ID)
		}
	}
}

func containsID(items []domain.RestaurantListItem, id uuid.UUID) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func ids(items []domain.RestaurantListItem) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}
