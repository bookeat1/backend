package venuefeature

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func freshRepo(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_venue_features", "venue_feature_aliases", "venue_features", "restaurants")
	return New(pool), pool
}

func newFeature(code, name string) *domain.VenueFeature {
	return &domain.VenueFeature{ID: uuid.New(), Code: code, Name: name, IsActive: true}
}

func seedVenue(t *testing.T, pool *pgxpool.Pool, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,'Venue','Алматы','₸₸',$2)`, id, active); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	return id
}

// TestDuplicateSpellingsAreRejectedByTheDatabase: the unique index on the
// NORMALIZED name is the entire reason this dictionary cannot rot the way the
// free-text restaurant_features table did. Two admins racing must not both
// succeed, so the guard has to be the index — not a read-then-write check.
func TestDuplicateSpellingsAreRejectedByTheDatabase(t *testing.T) {
	repo, _ := freshRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newFeature("terrace", "Терраса")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, dup := range []*domain.VenueFeature{
		newFeature("terrace", "Совсем другое"), // same code
		newFeature("terrace2", "терраса"),      // same name, different case
		newFeature("terrace3", "  Терраса  "),  // same name, padded
	} {
		if err := repo.Create(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("Create(%q/%q) = %v, want ErrAlreadyExists", dup.Code, dup.Name, err)
		}
	}
}

// TestListCountsOnlyActiveVenues pins venue_count. It exists so the owner can
// see which features nobody carries yet (and, later, measure demand), so a
// count that quietly included hidden venues would overstate exactly the number
// he is using to decide.
func TestListCountsOnlyActiveVenues(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	wifi := newFeature("wifi", "Wi-Fi")
	halal := newFeature("halal", "Халал")
	for _, f := range []*domain.VenueFeature{wifi, halal} {
		if err := repo.Create(ctx, f); err != nil {
			t.Fatalf("create %s: %v", f.Code, err)
		}
	}
	live := seedVenue(t, pool, true)
	hidden := seedVenue(t, pool, false)
	if err := repo.SetForRestaurant(ctx, live, []uuid.UUID{wifi.ID}); err != nil {
		t.Fatalf("assign to live venue: %v", err)
	}
	if err := repo.SetForRestaurant(ctx, hidden, []uuid.UUID{wifi.ID}); err != nil {
		t.Fatalf("assign to hidden venue: %v", err)
	}

	items, err := repo.List(ctx, domain.VenueFeatureFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byCode := map[string]int{}
	for _, f := range items {
		byCode[f.Code] = f.VenueCount
	}
	if byCode["wifi"] != 1 {
		t.Errorf("wifi venue_count = %d, want 1 (the hidden venue must not count)", byCode["wifi"])
	}
	// A feature nobody carries is still LISTED with a zero — the owner fills
	// the data by hand and asked for the empty ones to stay visible.
	if got, ok := byCode["halal"]; !ok || got != 0 {
		t.Errorf("halal venue_count = %d (present=%v), want a listed 0", got, ok)
	}
}

// TestSetForRestaurantReplacesAndOrders: assigning is a REPLACE, and the given
// order is preserved — a venue's set is a statement, not an increment.
func TestSetForRestaurantReplacesAndOrders(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	wifi := newFeature("wifi", "Wi-Fi")
	terrace := newFeature("terrace", "Терраса")
	parking := newFeature("parking", "Парковка")
	for _, f := range []*domain.VenueFeature{wifi, terrace, parking} {
		if err := repo.Create(ctx, f); err != nil {
			t.Fatalf("create %s: %v", f.Code, err)
		}
	}
	venue := seedVenue(t, pool, true)

	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{wifi.ID, terrace.ID}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	// Replace with a different set in a different order.
	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{parking.ID, wifi.ID}); err != nil {
		t.Fatalf("second set: %v", err)
	}

	byVenue, err := repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil {
		t.Fatalf("list by restaurants: %v", err)
	}
	got := byVenue[venue]
	if len(got) != 2 || got[0].Code != "parking" || got[1].Code != "wifi" {
		t.Fatalf("features = %+v, want [parking wifi] in that order", got)
	}
}

// TestResolveIDsRejectsUnknown: an id that does not exist fails the WHOLE call.
// Dropping it would answer "saved" for a set the venue never chose.
func TestResolveIDsRejectsUnknown(t *testing.T) {
	repo, _ := freshRepo(t)
	ctx := context.Background()

	wifi := newFeature("wifi", "Wi-Fi")
	if err := repo.Create(ctx, wifi); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.ResolveIDs(ctx, []uuid.UUID{wifi.ID, uuid.New()}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("ResolveIDs with an unknown id = %v, want ErrValidation", err)
	}
}

// TestHiddenFeatureStaysOnVenuesThatHaveIt: hiding stops a feature SPREADING,
// it does not silently strip it off venues that already carry it. Otherwise
// hiding one entry would rewrite venue data nobody asked to change.
func TestHiddenFeatureStaysOnVenuesThatHaveIt(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	wifi := newFeature("wifi", "Wi-Fi")
	if err := repo.Create(ctx, wifi); err != nil {
		t.Fatalf("create: %v", err)
	}
	venue := seedVenue(t, pool, true)
	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{wifi.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	wifi.IsActive = false
	if err := repo.Update(ctx, wifi); err != nil {
		t.Fatalf("hide: %v", err)
	}

	byVenue, err := repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil {
		t.Fatalf("list by restaurants: %v", err)
	}
	if len(byVenue[venue]) != 1 {
		t.Errorf("venue features after hiding = %+v, want the venue to keep what it had", byVenue[venue])
	}
	// ...and it disappears from the PUBLIC dictionary listing.
	items, err := repo.List(ctx, domain.VenueFeatureFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, f := range items {
		if f.Code == "wifi" {
			t.Error("a hidden feature is still in the public dictionary listing")
		}
	}
	// ...but the platform's own listing still shows it, or it could never be
	// brought back.
	admin, err := repo.List(ctx, domain.VenueFeatureFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	found := false
	for _, f := range admin {
		if f.Code == "wifi" {
			found = true
		}
	}
	if !found {
		t.Error("IncludeInactive listing lost the hidden feature: it would be unrecoverable")
	}
}

// TestFeatureInUseCannotBeDeleted pins the RESTRICT foreign key: the API has no
// hard delete, and the database must not have one either.
func TestFeatureInUseCannotBeDeleted(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	wifi := newFeature("wifi", "Wi-Fi")
	if err := repo.Create(ctx, wifi); err != nil {
		t.Fatalf("create: %v", err)
	}
	venue := seedVenue(t, pool, true)
	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{wifi.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM venue_features WHERE id = $1`, wifi.ID); err == nil {
		t.Error("deleting a feature carried by a venue succeeded, want the RESTRICT foreign key to refuse")
	}
}
