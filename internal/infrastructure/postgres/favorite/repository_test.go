package favorite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

// seedUserRow and seedRestaurantRow satisfy the foreign keys
// restaurant_favorites needs, going through each entity's own repository
// (same pattern as booking/*_test.go's seed helpers) rather than raw SQL.
func seedUserRow(ctx context.Context, t *testing.T, pool sqltx.Querier) uuid.UUID {
	t.Helper()
	repo := userrepo.New(pool)
	u := &domain.User{ID: uuid.New(), FullName: "Fav User", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func seedRestaurantRow(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{
		ID: uuid.New(), Name: name, City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func TestAddIsIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid := seedUserRow(ctx, t, pool)
	rid := seedRestaurantRow(ctx, t, pool, "Bistro A")

	repo := New(pool)
	if err := repo.Add(ctx, uid, rid); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := repo.Add(ctx, uid, rid); err != nil {
		t.Fatalf("second Add (idempotent) should not error: %v", err)
	}

	items, err := repo.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 favorite after two Adds, got %d", len(items))
	}
}

func TestAddUnknownRestaurantReturnsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid := seedUserRow(ctx, t, pool)
	repo := New(pool)

	if err := repo.Add(ctx, uid, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid := seedUserRow(ctx, t, pool)
	rid := seedRestaurantRow(ctx, t, pool, "Bistro B")
	repo := New(pool)

	// Removing something never favorited must not error.
	if err := repo.Remove(ctx, uid, rid); err != nil {
		t.Fatalf("Remove on non-favorited: %v", err)
	}

	if err := repo.Add(ctx, uid, rid); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := repo.Remove(ctx, uid, rid); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := repo.Remove(ctx, uid, rid); err != nil {
		t.Fatalf("second Remove (idempotent) should not error: %v", err)
	}

	items, err := repo.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 favorites after Remove, got %d", len(items))
	}
}

func TestListByUserIsolatesPerUser(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid1 := seedUserRow(ctx, t, pool)
	uid2 := seedUserRow(ctx, t, pool)
	rid1 := seedRestaurantRow(ctx, t, pool, "Only user1's")
	rid2 := seedRestaurantRow(ctx, t, pool, "Only user2's")

	repo := New(pool)
	if err := repo.Add(ctx, uid1, rid1); err != nil {
		t.Fatalf("Add uid1/rid1: %v", err)
	}
	if err := repo.Add(ctx, uid2, rid2); err != nil {
		t.Fatalf("Add uid2/rid2: %v", err)
	}

	items1, err := repo.ListByUser(ctx, uid1)
	if err != nil {
		t.Fatalf("ListByUser uid1: %v", err)
	}
	if len(items1) != 1 || items1[0].Restaurant.ID != rid1 {
		t.Fatalf("uid1 favorites leaked another user's data: %+v", items1)
	}

	items2, err := repo.ListByUser(ctx, uid2)
	if err != nil {
		t.Fatalf("ListByUser uid2: %v", err)
	}
	if len(items2) != 1 || items2[0].Restaurant.ID != rid2 {
		t.Fatalf("uid2 favorites leaked another user's data: %+v", items2)
	}
}

// TestAddConcurrentRace fires many concurrent Add calls for the same
// (user, restaurant) pair — two devices double-tapping "favorite" at the same
// instant is a real client race. Every call must succeed (idempotent, not a
// unique-violation error) and exactly one row must exist afterward.
func TestAddConcurrentRace(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid := seedUserRow(ctx, t, pool)
	rid := seedRestaurantRow(ctx, t, pool, "Race Bistro")
	repo := New(pool)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repo.Add(ctx, uid, rid)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Add call %d: %v", i, err)
		}
	}

	items, err := repo.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 favorite after %d concurrent Adds, got %d", n, len(items))
	}
}

func TestFavoriteSet(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_favorites", "restaurants", "users")
	ctx := context.Background()

	uid := seedUserRow(ctx, t, pool)
	rid1 := seedRestaurantRow(ctx, t, pool, "Favorited")
	rid2 := seedRestaurantRow(ctx, t, pool, "Not favorited")

	repo := New(pool)
	if err := repo.Add(ctx, uid, rid1); err != nil {
		t.Fatalf("Add: %v", err)
	}

	set, err := repo.FavoriteSet(ctx, uid, []uuid.UUID{rid1, rid2})
	if err != nil {
		t.Fatalf("FavoriteSet: %v", err)
	}
	if !set[rid1] {
		t.Errorf("expected rid1 favorited")
	}
	if set[rid2] {
		t.Errorf("expected rid2 NOT favorited (map must never hold an explicit false)")
	}

	empty, err := repo.FavoriteSet(ctx, uid, nil)
	if err != nil {
		t.Fatalf("FavoriteSet(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("FavoriteSet(nil) = %v, want empty map", empty)
	}
}
