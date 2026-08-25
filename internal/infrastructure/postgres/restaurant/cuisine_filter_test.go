package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedCuisine inserts a dictionary entry plus its normalized-name alias, the
// same pair migration 0079 seeds.
func seedCuisine(t *testing.T, pool *pgxpool.Pool, code, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO cuisines (id, code, name) VALUES ($1,$2,$3)`, id, code, name); err != nil {
		t.Fatalf("seed cuisine %s: %v", code, err)
	}
	for _, alias := range []string{domain.NormalizeCuisineKey(name), code} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO cuisine_aliases (alias, cuisine_id) VALUES ($1,$2)
			 ON CONFLICT (alias) DO NOTHING`, alias, id); err != nil {
			t.Fatalf("seed alias %q: %v", alias, err)
		}
	}
	return id
}

func linkCuisines(t *testing.T, pool *pgxpool.Pool, venue uuid.UUID, ids ...uuid.UUID) {
	t.Helper()
	for i, id := range ids {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position) VALUES ($1,$2,$3)`,
			venue, id, i); err != nil {
			t.Fatalf("link cuisine: %v", err)
		}
	}
}

func idSet(items []domain.RestaurantListItem) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(items))
	for _, it := range items {
		out[it.Restaurant.ID] = true
	}
	return out
}

// TestSearchCuisineFilterThroughDictionary is the compatibility contract of the
// cuisine filter, stated as a test because a store build cannot be fixed after
// the fact.
//
// It covers, in one seeded catalog:
//   - a venue with SEVERAL cuisines is found by EACH of them;
//   - the filter is case-insensitive (before 0079, «Европейская» and
//     «европейская» were two different filter values);
//   - a venue whose composite spelling has NOT been mapped yet is still found
//     by its legacy string, so nothing disappears while the owner decides;
//   - several cuisines OR together.
func TestSearchCuisineFilterThroughDictionary(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_cuisines", "cuisine_aliases", "cuisines",
		"restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	european := seedCuisine(t, pool, "european", "Европейская")
	italian := seedCuisine(t, pool, "italian", "Итальянская")
	kazakh := seedCuisine(t, pool, "kazakh", "Казахская")

	// A venue with two cuisines — the case the old single-string column could
	// not express at all.
	both := seedSearch(t, repo, "Trattoria", "", "Итальянская, Европейская", domain.CityAlmaty)
	linkCuisines(t, pool, both, italian, european)

	onlyKazakh := seedSearch(t, repo, "Dastarkhan", "", "Казахская", domain.CityAlmaty)
	linkCuisines(t, pool, onlyKazakh, kazakh)

	// Not mapped to the dictionary: a composite spelling awaiting a human
	// decision. It keeps its string and no links.
	unmapped := seedSearch(t, repo, "Corner Cafe", "", "Кафе, европейская", domain.CityAlmaty)

	cases := []struct {
		name     string
		cuisines []string
		want     []uuid.UUID
	}{
		{"by the first of two cuisines", []string{"Итальянская"}, []uuid.UUID{both}},
		// «Европейская» also reaches the unmapped venue, because its composite
		// string is matched part by part. That is the intended improvement,
		// not a leak: «Кафе, европейская» IS европейская, and before 0079 the
		// exact-string filter found it under neither of its two words.
		{"by the second of two cuisines", []string{"Европейская"}, []uuid.UUID{both, unmapped}},
		{"lower case finds the same venue", []string{"европейская"}, []uuid.UUID{both, unmapped}},
		{"by machine code", []string{"italian"}, []uuid.UUID{both}},
		{"two cuisines OR together", []string{"Итальянская", "Казахская"}, []uuid.UUID{both, onlyKazakh}},
		// The old app sends back exactly the string it was given, commas and
		// all. The transport layer splits on commas, and «европейская» then
		// resolves through the dictionary — so the old client filters BETTER
		// than it used to, not worse.
		{"legacy composite chip, split by the transport", []string{"Кафе", "европейская"}, []uuid.UUID{both, unmapped}},
		// An unmapped venue is still reachable by its own stored string.
		{"unmapped venue by its legacy string", []string{"Кафе, европейская"}, []uuid.UUID{unmapped}},
		{"unknown cuisine finds nothing", []string{"Марсианская"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := repo.Search(ctx, domain.RestaurantSearchFilter{Cuisines: tc.cuisines})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if total != len(tc.want) {
				t.Fatalf("total = %d, want %d (%v)", total, len(tc.want), tc.want)
			}
			got := idSet(items)
			for _, id := range tc.want {
				if !got[id] {
					t.Errorf("venue %v missing from the result", id)
				}
			}
		})
	}
}

// TestListAndSearchCarryTheCuisineSet checks the structured field the new
// clients read, and that the legacy string is still populated next to it.
func TestListAndSearchCarryTheCuisineSet(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_cuisines", "cuisine_aliases", "cuisines",
		"restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	european := seedCuisine(t, pool, "european", "Европейская")
	italian := seedCuisine(t, pool, "italian", "Итальянская")

	venue := seedSearch(t, repo, "Trattoria", "", "Итальянская, Европейская", domain.CityAlmaty)
	// Order matters: italian is position 0, i.e. the venue's main cuisine.
	linkCuisines(t, pool, venue, italian, european)

	items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one venue, got %d", len(items))
	}
	got := items[0]
	if len(got.Cuisines) != 2 {
		t.Fatalf("listing carries %d cuisines, want 2", len(got.Cuisines))
	}
	if got.Cuisines[0].Code != "italian" {
		t.Errorf("first cuisine = %q, want the venue's own order (italian at position 0)", got.Cuisines[0].Code)
	}
	if got.Restaurant.CuisineType == "" {
		t.Error("cuisine_type is empty: the legacy single-string field must keep working for store builds")
	}

	agg, err := repo.GetByID(ctx, venue)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(agg.Cuisines) != 2 || agg.Cuisines[0].Code != "italian" {
		t.Errorf("detail cuisines = %+v, want the same two in the same order", agg.Cuisines)
	}
}
