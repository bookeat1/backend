package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestRestaurantCRUDAndList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	order := 1
	popular := true
	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Test Bistro", NameI18n: domain.I18n{"ru": "Бистро"},
		City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		IsActive: true, IsPopular: &popular, DisplayOrder: &order,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Test Bistro" || got.NameI18n["ru"] != "Бистро" || got.City != domain.CityAlmaty {
		t.Errorf("roundtrip mismatch: %+v", got.Restaurant)
	}

	items, total, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), IsPopular: &popular})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != m.ID {
		t.Errorf("list = %d items (total %d), want 1", len(items), total)
	}

	if err := repo.SetActive(ctx, m.ID, false); err != nil {
		t.Fatalf("set active: %v", err)
	}
	_, total, _ = repo.ListActive(ctx, domain.RestaurantFilter{})
	if total != 0 {
		t.Errorf("after deactivate total = %d, want 0", total)
	}

	if _, err := repo.GetByID(ctx, uuid.New()); err != domain.ErrNotFound {
		t.Errorf("missing get err = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }
