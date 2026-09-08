package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

func seedUser(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := userrepo.New(pool)
	u := &domain.User{ID: uuid.New(), FullName: name, Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func TestUpsert_EditsInPlace(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	uid := seedUser(ctx, t, pool, "Guest")
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	rv := &domain.Review{RestaurantID: rid, UserID: uid, Rating: 3, Body: "ok"}
	if err := repo.Upsert(ctx, rv); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first := rv.ID

	// Edit: same (restaurant, user) → same row, new rating/body.
	rv2 := &domain.Review{RestaurantID: rid, UserID: uid, Rating: 5, Body: "great now"}
	if err := repo.Upsert(ctx, rv2); err != nil {
		t.Fatalf("second upsert (edit): %v", err)
	}
	if rv2.ID != first {
		t.Fatalf("edit must keep the same review id: %s != %s", rv2.ID, first)
	}

	got, err := repo.GetOwn(ctx, rid, uid)
	if err != nil {
		t.Fatalf("GetOwn: %v", err)
	}
	if got.Rating != 5 || got.Body != "great now" {
		t.Fatalf("edit not persisted: %+v", got)
	}

	agg, err := repo.Aggregate(ctx, rid)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Count != 1 {
		t.Fatalf("edit must not create a second review, count=%d", agg.Count)
	}
}

func TestUpsert_UnknownRestaurantIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	uid := seedUser(ctx, t, pool, "Guest")
	repo := New(pool)

	err := repo.Upsert(ctx, &domain.Review{RestaurantID: uuid.New(), UserID: uid, Rating: 4})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown restaurant, got %v", err)
	}
}

func TestAggregate_AverageAndCountOverPublishedOnly(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Rated")
	repo := New(pool)

	// Five published reviews: 5,4,3,2,1 → avg 3.0, count 5.
	for i, rating := range []int{5, 4, 3, 2, 1} {
		uid := seedUser(ctx, t, pool, "U")
		_ = i
		if err := repo.Upsert(ctx, &domain.Review{RestaurantID: rid, UserID: uid, Rating: rating}); err != nil {
			t.Fatalf("upsert rating %d: %v", rating, err)
		}
	}
	// One hidden 1-star that must NOT count toward the aggregate.
	hidden := seedUser(ctx, t, pool, "Hidden")
	hrv := &domain.Review{RestaurantID: rid, UserID: hidden, Rating: 1}
	if err := repo.Upsert(ctx, hrv); err != nil {
		t.Fatalf("upsert hidden: %v", err)
	}
	if err := repo.SetStatus(ctx, hrv.ID, domain.ReviewHidden); err != nil {
		t.Fatalf("SetStatus hidden: %v", err)
	}

	agg, err := repo.Aggregate(ctx, rid)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Count != 5 {
		t.Fatalf("count must exclude the hidden review, got %d", agg.Count)
	}
	if agg.Average < 2.999 || agg.Average > 3.001 {
		t.Fatalf("average = %v, want 3.0", agg.Average)
	}

	// A restaurant with no reviews at all → {0, 0}, never a NULL scan error.
	empty := seedRestaurant(ctx, t, pool, "Unrated")
	agg0, err := repo.Aggregate(ctx, empty)
	if err != nil {
		t.Fatalf("Aggregate(empty): %v", err)
	}
	if agg0.Count != 0 || agg0.Average != 0 {
		t.Fatalf("unrated restaurant must be {0,0}, got %+v", agg0)
	}
}

func TestListPublished_ExcludesHiddenAndPaginates(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Listed")
	repo := New(pool)

	var hiddenID uuid.UUID
	for i := 0; i < 3; i++ {
		uid := seedUser(ctx, t, pool, "Author")
		rv := &domain.Review{RestaurantID: rid, UserID: uid, Rating: 4, Body: "nice"}
		if err := repo.Upsert(ctx, rv); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if i == 1 {
			hiddenID = rv.ID
		}
	}
	if err := repo.SetStatus(ctx, hiddenID, domain.ReviewHidden); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	items, total, err := repo.ListPublished(ctx, rid, 1, 10)
	if err != nil {
		t.Fatalf("ListPublished: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("hidden review must be excluded: total=%d len=%d", total, len(items))
	}
	for _, it := range items {
		if it.ID == hiddenID {
			t.Fatal("hidden review leaked into the public listing")
		}
		if it.AuthorName != "Author" {
			t.Fatalf("author name not joined: %q", it.AuthorName)
		}
		if it.Status != domain.ReviewPublished {
			t.Fatalf("listing must only contain published reviews, got %s", it.Status)
		}
	}

	// Page 1 of size 1 then page 2 of size 1 must not overlap (stable order).
	p1, _, err := repo.ListPublished(ctx, rid, 1, 1)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	p2, _, err := repo.ListPublished(ctx, rid, 2, 1)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(p1) != 1 || len(p2) != 1 || p1[0].ID == p2[0].ID {
		t.Fatalf("pagination overlap or wrong sizes: p1=%v p2=%v", p1, p2)
	}
}

func TestSetReply_And_Pairing(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	uid := seedUser(ctx, t, pool, "Guest")
	rid := seedRestaurant(ctx, t, pool, "Replied")
	repo := New(pool)

	rv := &domain.Review{RestaurantID: rid, UserID: uid, Rating: 5, Body: "loved it"}
	if err := repo.Upsert(ctx, rv); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.SetReply(ctx, rv.ID, "thank you!", time.Now()); err != nil {
		t.Fatalf("SetReply: %v", err)
	}
	got, err := repo.GetByID(ctx, rv.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.OwnerReply == nil || *got.OwnerReply != "thank you!" || got.RepliedAt == nil {
		t.Fatalf("reply/replied_at not paired: %+v", got)
	}
}

func TestModerationAndReply_NotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	repo := New(pool)

	if err := repo.SetStatus(ctx, uuid.New(), domain.ReviewHidden); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetStatus on missing review: expected ErrNotFound, got %v", err)
	}
	if err := repo.SetReply(ctx, uuid.New(), "x", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetReply on missing review: expected ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByID on missing review: expected ErrNotFound, got %v", err)
	}
	if _, err := repo.GetOwn(ctx, uuid.New(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetOwn on missing review: expected ErrNotFound, got %v", err)
	}
}

func TestDeleteOwn_Idempotent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "reviews", "restaurants", "users")
	ctx := context.Background()
	uid := seedUser(ctx, t, pool, "Guest")
	rid := seedRestaurant(ctx, t, pool, "Deletable")
	repo := New(pool)

	// Deleting when nothing exists is a no-op.
	if err := repo.DeleteOwn(ctx, rid, uid); err != nil {
		t.Fatalf("delete non-existent: %v", err)
	}
	if err := repo.Upsert(ctx, &domain.Review{RestaurantID: rid, UserID: uid, Rating: 3}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.DeleteOwn(ctx, rid, uid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.DeleteOwn(ctx, rid, uid); err != nil {
		t.Fatalf("second delete (idempotent): %v", err)
	}
	if _, err := repo.GetOwn(ctx, rid, uid); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("review should be gone, got %v", err)
	}
}
