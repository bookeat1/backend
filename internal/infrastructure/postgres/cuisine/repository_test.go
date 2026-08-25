package cuisine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func freshRepo(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_cuisines", "cuisine_aliases", "cuisines", "restaurants")
	return New(pool), pool
}

func newCuisine(code, name string) *domain.Cuisine {
	return &domain.Cuisine{ID: uuid.New(), Code: code, Name: name, IsActive: true}
}

func seedVenue(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'Venue','Алматы','₸₸')`,
		id); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	return id
}

// TestDuplicateSpellingsAreRejectedByTheDatabase: the unique index on the
// NORMALIZED name is the entire reason the dictionary cannot rot the way the
// free-text column did. Two admins racing must not both succeed, so the guard
// has to be the index — not a read-then-write check in Go.
func TestDuplicateSpellingsAreRejectedByTheDatabase(t *testing.T) {
	repo, _ := freshRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newCuisine("european", "Европейская")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, dup := range []*domain.Cuisine{
		newCuisine("european", "Совсем другая"), // same code
		newCuisine("euro2", "европейская"),      // same name, different case
		newCuisine("euro3", "  Европейская  "),  // same name, padded
	} {
		if err := repo.Create(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("Create(%q/%q) = %v, want ErrAlreadyExists", dup.Code, dup.Name, err)
		}
	}
}

// TestSetForRestaurantReplacesAndOrders: assigning is a REPLACE, and the given
// order is stored as the position, because the first cuisine is the one a card
// with room for a single line shows.
func TestSetForRestaurantReplacesAndOrders(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()
	venue := seedVenue(t, pool)

	italian, european, kazakh := newCuisine("italian", "Итальянская"),
		newCuisine("european", "Европейская"), newCuisine("kazakh", "Казахская")
	for _, c := range []*domain.Cuisine{italian, european, kazakh} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.Code, err)
		}
	}

	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{italian.ID, european.ID}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got[venue]) != 2 || got[venue][0].Code != "italian" {
		t.Fatalf("set = %+v, want italian first", got[venue])
	}

	// Replacing with an overlapping set drops what is gone and reorders the
	// rest — the ON CONFLICT branch, which a plain INSERT would trip over.
	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{kazakh.ID, italian.ID}); err != nil {
		t.Fatalf("second set: %v", err)
	}
	got, err = repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(got[venue]) != 2 || got[venue][0].Code != "kazakh" || got[venue][1].Code != "italian" {
		t.Fatalf("set after replace = %+v, want [kazakh italian]", got[venue])
	}

	// Clearing.
	if err := repo.SetForRestaurant(ctx, venue, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil || len(got[venue]) != 0 {
		t.Fatalf("set after clear = %+v (%v), want empty", got[venue], err)
	}
}

// TestSetForRestaurantRollsBackInsideATx: SetForRestaurant deletes first and
// inserts after, so a bad id in the middle must take the delete down with it.
// Without the surrounding transaction the venue would silently end up with NO
// cuisines — the same failure mode usercuisine.Replace documents.
func TestSetForRestaurantRollsBackInsideATx(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()
	venue := seedVenue(t, pool)

	italian := newCuisine("italian", "Итальянская")
	if err := repo.Create(ctx, italian); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{italian.ID}); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		return repo.SetForRestaurant(ctx, venue, []uuid.UUID{uuid.New()})
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("set with an unknown cuisine = %v, want ErrValidation", err)
	}

	got, err := repo.ListByRestaurants(ctx, []uuid.UUID{venue})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got[venue]) != 1 || got[venue][0].Code != "italian" {
		t.Errorf("set after a rolled-back replace = %+v, want the previous set intact", got[venue])
	}
}

// TestHiddenCuisineListingAndDeleteProtection covers both halves of "скрыть":
// a hidden entry disappears from the public list but stays visible to the
// platform, and a HARD delete of a cuisine a venue uses is refused by the
// database itself (ON DELETE RESTRICT) rather than quietly emptying venues.
// That refusal is exactly why the API has no hard delete at all.
func TestHiddenCuisineListingAndDeleteProtection(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()
	venue := seedVenue(t, pool)

	active, hidden := newCuisine("european", "Европейская"), newCuisine("coffee", "Кофейня")
	hidden.IsActive = false
	for _, c := range []*domain.Cuisine{active, hidden} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.Code, err)
		}
	}

	public, err := repo.List(ctx, domain.CuisineFilter{})
	if err != nil || len(public) != 1 || public[0].Code != "european" {
		t.Fatalf("public list = %+v (%v), want only the active entry", public, err)
	}
	all, err := repo.List(ctx, domain.CuisineFilter{IncludeInactive: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("admin list = %+v (%v), want both", all, err)
	}

	if err := repo.SetForRestaurant(ctx, venue, []uuid.UUID{active.ID}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cuisines WHERE id = $1`, active.ID); err == nil {
		t.Error("deleting a cuisine a venue uses succeeded; the FK must be RESTRICT so hiding is the only way")
	}
}
