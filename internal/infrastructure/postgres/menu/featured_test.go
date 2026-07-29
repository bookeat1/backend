package menu

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedDish inserts one dish and returns its id. Every flag that decides whether
// the rail may show it is a parameter, because those flags are what this file
// is about.
func seedDish(t *testing.T, ctx context.Context, repo *Repository, rid uuid.UUID, name string, available, featured bool) uuid.UUID {
	t.Helper()
	m := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: name, Price: "1000.00",
		IsAvailable: available,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create dish %s: %v", name, err)
	}
	if featured {
		if err := repo.SetFeatured(ctx, rid, m.ID, true); err != nil {
			t.Fatalf("feature dish %s: %v", name, err)
		}
	}
	return m.ID
}

// The rail is a city feed of picked dishes. Everything it must exclude is
// seeded here alongside the one row it must return, so a regression that
// loosens any predicate fails on a concrete dish rather than on a count.
func TestListFeaturedOnlyShowsPickedAvailableDishesOfActiveVenuesInCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	almaty, astana, hidden := uuid.New(), uuid.New(), uuid.New()
	for _, v := range []struct {
		id     uuid.UUID
		name   string
		city   string
		active bool
	}{
		{almaty, "Almaty venue", "Алматы", true},
		{astana, "Astana venue", "Астана", true},
		{hidden, "Hidden venue", "Алматы", false},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category, is_active)
			 VALUES ($1,$2,$3,'₸',$4)`, v.id, v.name, v.city, v.active); err != nil {
			t.Fatalf("seed venue %s: %v", v.name, err)
		}
	}

	wanted := seedDish(t, ctx, repo, almaty, "Picked and available", true, true)
	seedDish(t, ctx, repo, almaty, "Picked but sold out", false, true)
	seedDish(t, ctx, repo, almaty, "Available but not picked", true, false)
	seedDish(t, ctx, repo, astana, "Picked in another city", true, true)
	seedDish(t, ctx, repo, hidden, "Picked in a hidden venue", true, true)

	got, err := repo.ListFeatured(ctx, domain.FeaturedMenuFilter{City: "Алматы", Limit: 10})
	if err != nil {
		t.Fatalf("list featured: %v", err)
	}
	if len(got) != 1 {
		names := make([]string, 0, len(got))
		for _, g := range got {
			names = append(names, g.Item.Name)
		}
		t.Fatalf("want exactly the picked available dish of the active Almaty venue, got %d: %v", len(got), names)
	}
	if got[0].Item.ID != wanted {
		t.Fatalf("wrong dish: got %q", got[0].Item.Name)
	}
	if !got[0].Item.IsFeatured {
		t.Fatal("is_featured must round-trip through the read")
	}
	if got[0].RestaurantName != "Almaty venue" {
		t.Fatalf("card must carry the venue name, got %q", got[0].RestaurantName)
	}
}

// A stop-listed dish keeps its pick: staff should not have to re-pick a dish
// after every "we ran out", so the flag survives and the row simply returns to
// the rail once it is available again.
func TestFeaturedPickSurvivesAStopList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	id := seedDish(t, ctx, repo, rid, "Plov", true, true)

	if err := repo.SetAvailable(ctx, id, false); err != nil {
		t.Fatalf("stop list: %v", err)
	}
	got, err := repo.ListFeatured(ctx, domain.FeaturedMenuFilter{City: "Алматы", Limit: 10})
	if err != nil {
		t.Fatalf("list featured: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a sold-out dish must leave the rail, got %d", len(got))
	}

	if err := repo.SetAvailable(ctx, id, true); err != nil {
		t.Fatalf("back in stock: %v", err)
	}
	got, err = repo.ListFeatured(ctx, domain.FeaturedMenuFilter{City: "Алматы", Limit: 10})
	if err != nil {
		t.Fatalf("list featured: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the pick must survive the stop list and come back, got %d", len(got))
	}
}

// The tenant guard is the whole point of taking restaurantID here: a manager of
// one venue must not be able to promote another venue's dish onto the shared
// main screen by guessing an id.
func TestSetFeaturedRefusesADishOfAnotherVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	mine, theirs := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{mine, theirs} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, id); err != nil {
			t.Fatalf("seed venue: %v", err)
		}
	}
	theirDish := seedDish(t, ctx, repo, theirs, "Their plov", true, false)

	err := repo.SetFeatured(ctx, mine, theirDish, true)
	if err == nil {
		t.Fatal("promoting another venue's dish must fail")
	}
	if err != domain.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	got, err := repo.GetByID(ctx, theirDish)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IsFeatured {
		t.Fatal("the dish must NOT have been promoted")
	}
}

// Limit is what keeps a home-screen rail from becoming a catalogue dump.
func TestListFeaturedRespectsLimit(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	for _, n := range []string{"one", "two", "three"} {
		seedDish(t, ctx, repo, rid, n, true, true)
	}

	got, err := repo.ListFeatured(ctx, domain.FeaturedMenuFilter{City: "Алматы", Limit: 2})
	if err != nil {
		t.Fatalf("list featured: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
}
