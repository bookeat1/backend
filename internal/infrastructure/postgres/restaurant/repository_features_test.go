package restaurant

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/postgres/venuefeature"
)

// featuresFixture builds three venues with known feature sets:
//
//	both     — Wi-Fi AND parking
//	wifiOnly — Wi-Fi
//	bare     — nothing
//
// That is the minimum needed to tell AND from OR: under OR, a query for
// "wifi + parking" returns both venues; under AND, only the first.
type featuresFixture struct {
	both, wifiOnly, bare uuid.UUID
}

func seedFeatureFixture(t *testing.T) (*Repository, featuresFixture) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_venue_features", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	feats := venuefeature.New(pool)

	wifi := ensureFeature(t, pool, "wifi", "Wi-Fi")
	parking := ensureFeature(t, pool, "parking", "Парковка")

	mk := func(name string, ids ...uuid.UUID) uuid.UUID {
		id := uuid.New()
		if err := repo.Create(ctx, &domain.Restaurant{
			ID: id, Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceLow, IsActive: true,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if len(ids) > 0 {
			if err := feats.SetForRestaurant(ctx, id, ids); err != nil {
				t.Fatalf("assign features to %s: %v", name, err)
			}
		}
		return id
	}
	return repo, featuresFixture{
		both:     mk("Both", wifi, parking),
		wifiOnly: mk("WifiOnly", wifi),
		bare:     mk("Bare"),
	}
}

// ensureFeature returns the dictionary entry for code, creating it (with its
// name and code aliases) if it is not there.
//
// It does NOT simply read what migration 0082 seeded, deliberately: these
// tests share one database with every other package, and the venuefeature
// repository tests truncate the dictionary to isolate themselves. Depending on
// the seed surviving that would make this file pass or fail according to test
// ORDER — the exact cross-package flake class already recorded for this suite.
// What the migration actually seeds is pinned where it belongs, in
// venuefeature/migration_test.go.
func ensureFeature(t *testing.T, pool *pgxpool.Pool, code, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id FROM venue_features WHERE code = $1`, code).Scan(&id)
	if err != nil {
		id = uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO venue_features (id, code, name) VALUES ($1,$2,$3)`, id, code, name); err != nil {
			t.Fatalf("seed feature %q: %v", code, err)
		}
	}
	// The aliases are re-asserted even when the entry already existed: the
	// dictionary and its alias table are truncated INDEPENDENTLY by the
	// venuefeature package's own tests, and a feature that survived while its
	// aliases did not matches nothing — which reads exactly like a broken
	// filter instead of a broken fixture.
	for _, alias := range []string{strings.ToLower(strings.TrimSpace(name)), code} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO venue_feature_aliases (alias, feature_id) VALUES ($1,$2)
			 ON CONFLICT (alias) DO NOTHING`, alias, id); err != nil {
			t.Fatalf("seed alias %q: %v", alias, err)
		}
	}
	return id
}

func searchIDs(t *testing.T, repo *Repository, f domain.RestaurantSearchFilter) []uuid.UUID {
	t.Helper()
	items, total, err := repo.Search(context.Background(), f)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != len(items) {
		t.Errorf("total = %d but %d items came back: the count and the page disagree", total, len(items))
	}
	out := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func sortedIDs(ids ...uuid.UUID) []uuid.UUID {
	out := append([]uuid.UUID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSearchFeatureFilterIsAND is the load-bearing test of the whole feature.
//
// A guest who ticks «Намазхана» and «Парковка» is asking for a place with BOTH
// (owner's decision, 2026-08-25). Cuisine is the opposite — an OR-set — so the
// two must not be allowed to drift into the same implementation by accident.
func TestSearchFeatureFilterIsAND(t *testing.T) {
	repo, fx := seedFeatureFixture(t)

	// One feature: everything that carries it.
	got := searchIDs(t, repo, domain.RestaurantSearchFilter{Features: []string{"wifi"}})
	if want := sortedIDs(fx.both, fx.wifiOnly); !sameIDs(got, want) {
		t.Errorf("?features=wifi = %v, want %v", got, want)
	}

	// Two features: ONLY the venue that carries both. If this returns two
	// venues, the filter has become an OR-set and the guest is being shown a
	// place that does not answer the question they asked.
	got = searchIDs(t, repo, domain.RestaurantSearchFilter{Features: []string{"wifi", "parking"}})
	if want := sortedIDs(fx.both); !sameIDs(got, want) {
		t.Errorf("?features=wifi,parking = %v, want only the venue with both (%v)", got, want)
	}
}

// TestSearchFeatureFilterAcceptsCodeNameAndAlias: the query may carry the code,
// the Russian name, or any approved alias, in any case. The app should send
// codes, but an older client sends whatever label it scraped.
func TestSearchFeatureFilterAcceptsCodeNameAndAlias(t *testing.T) {
	repo, fx := seedFeatureFixture(t)
	want := sortedIDs(fx.both, fx.wifiOnly)

	for _, key := range []string{"wifi", "Wi-Fi", "wi-fi", "  WI-FI  "} {
		if got := searchIDs(t, repo, domain.RestaurantSearchFilter{Features: []string{key}}); !sameIDs(got, want) {
			t.Errorf("?features=%q = %v, want %v", key, got, want)
		}
	}
	// A comma-separated pair arriving as ONE string is split by the transport,
	// so the repository sees two keys — check the repository half here.
	if got := searchIDs(t, repo, domain.RestaurantSearchFilter{
		Features: []string{"Терраса"}, // an approved name nobody in the fixture has
	}); len(got) != 0 {
		t.Errorf("?features=Терраса = %v, want nothing: no fixture venue has a terrace", got)
	}
}

// TestSearchUnknownFeatureReturnsNothing: an unknown key must narrow to zero,
// never be dropped. A silently ignored filter is how the catalog once answered
// «открыто сейчас» with the entire catalog — see
// bugs/bookeat-backend-catalog-filters-silently-ignored.md.
func TestSearchUnknownFeatureReturnsNothing(t *testing.T) {
	repo, _ := seedFeatureFixture(t)
	got := searchIDs(t, repo, domain.RestaurantSearchFilter{Features: []string{"helipad"}})
	if len(got) != 0 {
		t.Errorf("?features=helipad = %v, want nothing (and definitely not the whole catalog)", got)
	}
}

// TestListActiveFeatureFilterIsAND: the catalog listing carries the same filter
// with the same semantics. Two endpoints that disagree about what a filter
// means is the bug this whole vertical exists to stop repeating.
func TestListActiveFeatureFilterIsAND(t *testing.T) {
	repo, fx := seedFeatureFixture(t)
	ctx := context.Background()

	items, total, err := repo.ListActive(ctx, domain.RestaurantFilter{Features: []string{"wifi", "parking"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Restaurant.ID != fx.both {
		t.Fatalf("catalog listing with two features returned %d rows (total %d), want only the venue carrying both", len(items), total)
	}
	// ...and it carries its features in the payload, so a card can show why it
	// matched.
	if len(items[0].Features) != 2 {
		t.Errorf("listing item features = %+v, want both to be loaded", items[0].Features)
	}
}

// TestGetByIDLoadsDictionaryFeatures: the detail read serves features from the
// dictionary now that the free-text table is gone. The JSON field it feeds
// (`features[]`) is unchanged, so this is the test that keeps that promise.
func TestGetByIDLoadsDictionaryFeatures(t *testing.T) {
	repo, fx := seedFeatureFixture(t)
	agg, err := repo.GetByID(context.Background(), fx.both)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(agg.Features) != 2 {
		t.Fatalf("features = %+v, want the two assigned dictionary entries", agg.Features)
	}
	if agg.Features[0].Code == "" {
		t.Error("feature code is empty: the filter and the client icon both key off it")
	}
	bare, err := repo.GetByID(context.Background(), fx.bare)
	if err != nil {
		t.Fatalf("get bare: %v", err)
	}
	if len(bare.Features) != 0 {
		t.Errorf("a venue with no features got %+v", bare.Features)
	}
}
