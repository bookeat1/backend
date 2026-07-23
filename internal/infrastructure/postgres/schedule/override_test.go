package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func ptr[T any](v T) *T { return &v }

func TestScheduleOverrideUpsertListDelete(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// closed-day override
	closed := &domain.ScheduleOverride{RestaurantID: rid, Date: day, IsClosed: true, Note: ptr("New Year")}
	if err := repo.Upsert(ctx, closed); err != nil {
		t.Fatalf("upsert closed: %v", err)
	}
	// idempotent upsert on the same (restaurant, date): flip to an open, custom-hours day
	open := &domain.ScheduleOverride{
		RestaurantID: rid, Date: day, IsClosed: false, OpenTime: ptr("12:00"), CloseTime: ptr("18:00"),
	}
	if err := repo.Upsert(ctx, open); err != nil {
		t.Fatalf("upsert open (replace): %v", err)
	}

	list, err := repo.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("upsert should replace not duplicate: got %d rows", len(list))
	}
	got := list[0]
	if got.IsClosed || got.OpenTime == nil || *got.OpenTime != "12:00" || *got.CloseTime != "18:00" {
		t.Errorf("replaced override mismatch: %+v", got)
	}

	// CHECK-constraint mapping: an "open" override with no times → ErrValidation
	bad := &domain.ScheduleOverride{RestaurantID: rid, Date: day.AddDate(0, 0, 1), IsClosed: false}
	if err := repo.Upsert(ctx, bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad override: got %v, want ErrValidation", err)
	}

	// delete reverts the day; deleting again is ErrNotFound
	if err := repo.Delete(ctx, rid, day); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Delete(ctx, rid, day); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}
