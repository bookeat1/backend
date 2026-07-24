package promo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func mkPromo(rid uuid.UUID, status domain.PromoStatus, startsIn, dur time.Duration) *domain.Promo {
	start := time.Now().Add(startsIn).UTC().Truncate(time.Second)
	return &domain.Promo{
		RestaurantID: rid,
		Title:        "P",
		StartsAt:     start,
		EndsAt:       start.Add(dur),
		Status:       status,
	}
}

func TestListActive_OnlyPublishedWithinWindow(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	// published, window contains now: SHOWN
	live := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	// published, not started yet: HIDDEN
	future := mkPromo(rid, domain.PromoPublished, time.Hour, 2*time.Hour)
	// published, already expired: HIDDEN
	expired := mkPromo(rid, domain.PromoPublished, -48*time.Hour, time.Hour)
	// draft within window: HIDDEN
	draft := mkPromo(rid, domain.PromoDraft, -time.Hour, 2*time.Hour)
	// hidden within window: HIDDEN
	hidden := mkPromo(rid, domain.PromoHidden, -time.Hour, 2*time.Hour)
	for _, p := range []*domain.Promo{live, future, expired, draft, hidden} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create promo: %v", err)
		}
	}

	items, total, err := repo.ListActive(ctx, rid, time.Now(), 1, 20)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly 1 active promo, got total=%d len=%d", total, len(items))
	}
	if items[0].ID != live.ID {
		t.Fatalf("wrong active promo returned: %s", items[0].ID)
	}
}

func TestListByRestaurant_StatusFilter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	for _, p := range []*domain.Promo{
		mkPromo(rid, domain.PromoDraft, time.Hour, time.Hour),
		mkPromo(rid, domain.PromoPublished, time.Hour, time.Hour),
		mkPromo(rid, domain.PromoHidden, time.Hour, time.Hour),
	} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// No filter → all 3.
	_, total, err := repo.ListByRestaurant(ctx, rid, nil, 1, 20)
	if err != nil || total != 3 {
		t.Fatalf("unfiltered admin list: total=%d err=%v", total, err)
	}
	// Filter to draft → 1.
	_, total, err = repo.ListByRestaurant(ctx, rid, []domain.PromoStatus{domain.PromoDraft}, 1, 20)
	if err != nil || total != 1 {
		t.Fatalf("draft-filtered admin list: total=%d err=%v", total, err)
	}
}
