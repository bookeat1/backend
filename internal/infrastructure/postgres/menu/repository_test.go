package menu

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func TestMenuItemCRUDListTagsAvailability(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	repo := New(pool)
	txm := sqltx.NewManager(pool)
	lang := "ru"
	order := 1
	m := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Plov", NameI18n: domain.I18n{"ru": "Плов"},
		Price: "3500.00", IsAvailable: true, Category: ptr("Основные"), Language: &lang, DisplayOrder: &order,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	// tags in a tx
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceTags(ctx, m.ID, []domain.MenuItemTag{{Tag: "halal"}, {Tag: "spicy"}})
	}); err != nil {
		t.Fatalf("replace tags: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Price != "3500.00" || got.NameI18n["ru"] != "Плов" || len(got.Tags) != 2 {
		t.Errorf("roundtrip mismatch: price=%q tags=%d", got.Price, len(got.Tags))
	}

	// language filter: nil → ru or null; "en" → none
	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid})
	if err != nil || len(items) != 1 || len(items[0].Tags) != 2 {
		t.Fatalf("list(default) = %d items err=%v", len(items), err)
	}
	en := "en"
	items, _ = repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid, Language: &en})
	if len(items) != 0 {
		t.Errorf("list(en) = %d, want 0", len(items))
	}

	if err := repo.SetAvailable(ctx, m.ID, false); err != nil {
		t.Fatalf("set available: %v", err)
	}
	got, _ = repo.GetByID(ctx, m.ID)
	if got.IsAvailable {
		t.Error("expected unavailable after SetAvailable(false)")
	}

	if err := repo.Delete(ctx, m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, m.ID); err != domain.ErrNotFound {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestMenuItemUpdate(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "menu_categories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	repo := New(pool)
	lang := "ru"
	order := 1
	m := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Plov", NameI18n: domain.I18n{"ru": "Плов"},
		Price: "3500.00", IsAvailable: true, Category: ptr("Основные"), Language: &lang, DisplayOrder: &order,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	created, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}

	m.Name = "Lagman"
	m.Price = "4200.00"
	if err := repo.Update(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "Lagman" || got.Price != "4200.00" {
		t.Errorf("update mismatch: name=%q price=%q", got.Name, got.Price)
	}
	if got.RestaurantID != created.RestaurantID {
		t.Errorf("restaurant_id changed: got %v, want %v", got.RestaurantID, created.RestaurantID)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed: got %v, want %v", got.CreatedAt, created.CreatedAt)
	}

	// positive language filter: item with Language="en" must be returned by ListByRestaurant(Language: "en").
	en := "en"
	enOrder := 2
	enItem := &domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: "Burger", Price: "2500.00",
		IsAvailable: true, Language: &en, DisplayOrder: &enOrder,
	}
	if err := repo.Create(ctx, enItem); err != nil {
		t.Fatalf("create en item: %v", err)
	}
	items, err := repo.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: rid, Language: &en})
	if err != nil {
		t.Fatalf("list(en): %v", err)
	}
	if len(items) != 1 || items[0].ID != enItem.ID {
		t.Fatalf("list(en) = %d items, want 1 matching enItem.ID", len(items))
	}
}

func TestMenuCategoryCRUD(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_categories")
	ctx := context.Background()
	repo := NewCategories(pool)

	c := &domain.MenuCategory{Name: "Основные", NameI18n: domain.I18n{"ru": "Основные"}, DisplayOrder: 1}
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	child := &domain.MenuCategory{Name: "Супы", ParentID: &c.ID, DisplayOrder: 2}
	if err := repo.Create(ctx, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d err=%v", len(list), err)
	}
	c.Name = "Горячее"
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := repo.Delete(ctx, child.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
