package events

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	cuisinerepo "backend-core/internal/infrastructure/postgres/cuisine"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	foodieoptionrepo "backend-core/internal/infrastructure/postgres/foodieoption"
	"backend-core/internal/infrastructure/postgres/foodieprofile"
	restaurantrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/usecase/tastematch"
)

// integrationTables lists every table this file's tests own, children first.
// Truncated before each test so one test's leftovers cannot pass another.
//
// foodie_options is included for the same reason
// usecase/tastematch/loader_test.go's own loaderTables truncates it: `go
// test ./...` runs packages concurrently against one shared Postgres, and
// infrastructure/postgres/foodieoption's tests also truncate this table —
// relying on the migration-0110 seed surviving until THIS package's tests
// run would make the outcome depend on inter-package test order/timing.
// integrationSeedCuisine below (re)creates exactly the tile row this file's
// one test needs.
var integrationTables = []string{
	"user_foodie_cuisines", "user_foodie_diets", "user_foodie_allergies",
	"events", "restaurant_cuisines", "cuisine_aliases", "cuisines",
	"foodie_options", "restaurants", "users",
}

// integrationSeedCuisine inserts a cuisine-dictionary entry. tileCodes are
// OPTIONAL — pass a wizard cuisine-tile code (usually the same string as
// code) to also link this cuisine to that tile's foodie_options row,
// creating the row if absent. Omit for a cuisine the guest's profile is not
// expected to match.
func integrationSeedCuisine(ctx context.Context, t *testing.T, pool sqltx.Querier, code string, tileCodes ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO cuisines (id, code, name) VALUES ($1,$2,$3)`, id, code, code); err != nil {
		t.Fatalf("seed cuisine %s: %v", code, err)
	}
	for _, tile := range tileCodes {
		integrationLinkFoodieCuisineTile(ctx, t, pool, tile, id)
	}
	return id
}

// integrationLinkFoodieCuisineTile ensures a cuisine-kind foodie_options row
// for tile exists and links it to cuisineID in foodie_option_cuisines.
func integrationLinkFoodieCuisineTile(ctx context.Context, t *testing.T, pool sqltx.Querier, tile string, cuisineID uuid.UUID) {
	t.Helper()
	var optionID uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO foodie_options (id, kind, code, name, is_active)
		 VALUES ($1, 'cuisine', $2, $2, true)
		 ON CONFLICT (kind, code) DO UPDATE SET updated_at = now()
		 RETURNING id`, uuid.New(), tile).Scan(&optionID)
	if err != nil {
		t.Fatalf("seed foodie option tile %s: %v", tile, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO foodie_option_cuisines (option_id, cuisine_id) VALUES ($1,$2)
		 ON CONFLICT (option_id, cuisine_id) DO NOTHING`, optionID, cuisineID); err != nil {
		t.Fatalf("link foodie cuisine tile %s: %v", tile, err)
	}
}

func integrationSeedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, cuisineIDs ...uuid.UUID) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := restaurantrepo.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for i, cid := range cuisineIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position) VALUES ($1,$2,$3)`,
			r.ID, cid, i); err != nil {
			t.Fatalf("link cuisine: %v", err)
		}
	}
	return r.ID
}

func integrationSeedEvent(ctx context.Context, t *testing.T, repo domain.EventRepository, rid uuid.UUID, startsIn time.Duration) *domain.Event {
	t.Helper()
	start := time.Now().Add(startsIn).UTC().Truncate(time.Second)
	e := &domain.Event{
		RestaurantID: &rid, Title: "E",
		StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.EventPublished,
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return e
}

// TestListPublicUpcomingForYou_Integration exercises the REAL stack end to
// end — postgres/event's repository, postgres/restaurant's bulk ListActive
// read, and usecase/tastematch's loader over a real Postgres — not fakes.
// Two venues score differently for one guest's explicit cuisine pick; the
// higher-scoring venue's event starts LATER but must still rank first under
// sort=for_you (criterion 19), while the plain listing (no sort=for_you)
// keeps today's date order unchanged — the same guarantee the unit tests in
// taste_match_test.go give with fakes, proven here against real SQL.
func TestListPublicUpcomingForYou_Integration(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, integrationTables...)
	ctx := context.Background()

	italianCuisineID := integrationSeedCuisine(ctx, t, pool, "italian", "italian")
	kazakhCuisineID := integrationSeedCuisine(ctx, t, pool, "kazakh")
	venueItalian := integrationSeedRestaurant(ctx, t, pool, "Osteria", italianCuisineID)
	venueKazakh := integrationSeedRestaurant(ctx, t, pool, "Дастархан", kazakhCuisineID)

	er := eventrepo.New(pool)
	// The matching venue's event starts LATER — if the listing were still
	// sorting by date alone this would come second, not first.
	matching := integrationSeedEvent(ctx, t, er, venueItalian, 48*time.Hour)
	soonerButNoMatch := integrationSeedEvent(ctx, t, er, venueKazakh, 2*time.Hour)

	guestID := uuid.New()
	if err := userrepo.New(pool).Create(ctx, &domain.User{
		ID: guestID, FullName: "Guest", Role: domain.RoleUser, PreferredLanguage: "ru",
	}); err != nil {
		t.Fatalf("seed guest: %v", err)
	}
	if err := foodieprofile.New(pool).Replace(ctx, guestID, domain.FoodieProfilePreferences{
		Cuisines: []string{domain.FoodieCuisineItalian},
	}); err != nil {
		t.Fatalf("seed foodie profile: %v", err)
	}

	restRepo := restaurantrepo.New(pool)
	loader := tastematch.NewLoader(foodieprofile.New(pool), userrepo.New(pool), cuisinerepo.New(pool), bookingrepo.New(pool), foodieoptionrepo.New(pool))
	f := NewFacade(er, nil, nil, WithTasteMatch(loader, restRepo, nil))

	// sort=for_you: score desc (matching venue wins) beats date order.
	ranked, total, err := f.ListPublicUpcomingForYou(ctx, domain.PublicEventFilter{City: cityPtr(string(domain.CityAlmaty)), Page: 1, PerPage: 20}, guestID)
	if err != nil {
		t.Fatalf("ListPublicUpcomingForYou: %v", err)
	}
	if total != 2 || len(ranked) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", total, len(ranked))
	}
	if ranked[0].ID != matching.ID || ranked[1].ID != soonerButNoMatch.ID {
		t.Fatalf("order = [%s, %s], want the matching (later) venue first", ranked[0].ID, ranked[1].ID)
	}
	if ranked[0].Match.Score <= ranked[1].Match.Score {
		t.Fatalf("scores = [%d, %d], want the first strictly higher", ranked[0].Match.Score, ranked[1].Match.Score)
	}
	if ranked[0].Match.Score != 400 {
		t.Fatalf("matching venue score = %d, want 400 (cuisine_match only)", ranked[0].Match.Score)
	}

	// Without sort=for_you (the plain listing): unchanged date order, the
	// sooner event first — proves this endpoint's default behaviour was not
	// disturbed by adding the ranked one.
	plain, plainTotal, err := f.ListPublicUpcoming(ctx, domain.PublicEventFilter{City: cityPtr(string(domain.CityAlmaty)), Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if plainTotal != 2 || len(plain) != 2 {
		t.Fatalf("plain total=%d len=%d, want 2/2", plainTotal, len(plain))
	}
	if plain[0].ID != soonerButNoMatch.ID || plain[1].ID != matching.ID {
		t.Fatalf("plain order = [%s, %s], want date order (sooner first)", plain[0].ID, plain[1].ID)
	}
}
