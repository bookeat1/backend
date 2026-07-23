package restaurant

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// assertLayoutData compares raw JSONB against an expected decoded value,
// since Postgres re-serializes jsonb (e.g. adds a space after ':') rather
// than preserving the original byte-for-byte input.
func assertLayoutData(t *testing.T, got json.RawMessage, want map[string]any) {
	t.Helper()
	var gotVal map[string]any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshal layout_data: %v", err)
	}
	if !reflect.DeepEqual(gotVal, want) {
		t.Errorf("layout_data = %v, want %v", gotVal, want)
	}
}

func TestRelatedReplaceAndRead(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rel := NewRelated(pool)
	txm := sqltx.NewManager(pool)

	rid := uuid.New()
	if err := repo.Create(ctx, &domain.Restaurant{
		ID: rid, Name: "X", City: domain.CityAstana, PriceCategory: domain.PriceLow, IsActive: true,
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}

	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := rel.ReplaceImages(ctx, rid, []domain.Image{{ImageURL: "a.png", IsPrimary: true}}); err != nil {
			return err
		}
		return rel.ReplaceFeatures(ctx, rid, []domain.Feature{{Name: "wifi", NameI18n: domain.I18n{"ru": "вайфай"}}})
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	agg, err := repo.GetByID(ctx, rid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(agg.Images) != 1 || agg.Images[0].ImageURL != "a.png" {
		t.Errorf("images = %+v", agg.Images)
	}
	if len(agg.Features) != 1 || agg.Features[0].NameI18n["ru"] != "вайфай" {
		t.Errorf("features = %+v", agg.Features)
	}
}

func TestUpsertFloorPlan(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rel := NewRelated(pool)

	rid := uuid.New()
	if err := repo.Create(ctx, &domain.Restaurant{
		ID: rid, Name: "Floor Plan Test", City: domain.CityAstana, PriceCategory: domain.PriceLow, IsActive: true,
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}

	fp := &domain.FloorPlan{RestaurantID: rid, LayoutData: json.RawMessage(`{"tables":1}`)}
	if err := rel.UpsertFloorPlan(ctx, fp); err != nil {
		t.Fatalf("upsert (insert): %v", err)
	}
	if fp.ID == uuid.Nil {
		t.Fatal("upsert did not assign an id")
	}

	got, err := rel.GetFloorPlan(ctx, rid)
	if err != nil {
		t.Fatalf("get floor plan: %v", err)
	}
	assertLayoutData(t, got.LayoutData, map[string]any{"tables": float64(1)})

	// Upsert again with a different id/layout: must hit ON CONFLICT and update
	// the existing row rather than erroring or duplicating.
	fp2 := &domain.FloorPlan{RestaurantID: rid, LayoutData: json.RawMessage(`{"tables":5}`)}
	if err := rel.UpsertFloorPlan(ctx, fp2); err != nil {
		t.Fatalf("upsert (conflict): %v", err)
	}

	got2, err := rel.GetFloorPlan(ctx, rid)
	if err != nil {
		t.Fatalf("get floor plan after conflict: %v", err)
	}
	assertLayoutData(t, got2.LayoutData, map[string]any{"tables": float64(5)})
}

func TestManagersCreateAndList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "users")
	ctx := context.Background()
	repo := New(pool)
	mgrs := NewManagers(pool)

	rid := uuid.New()
	if err := repo.Create(ctx, &domain.Restaurant{
		ID: rid, Name: "Manager Test", City: domain.CityAstana, PriceCategory: domain.PriceLow, IsActive: true,
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}

	uid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, full_name) VALUES ($1,$2,$3)`,
		uid, "manager@example.com", "Test Manager"); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	mn := &domain.RestaurantManager{RestaurantID: rid, UserID: uid, Role: domain.StaffRoleHostess}
	if err := mgrs.Create(ctx, mn); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	if mn.ID == uuid.Nil {
		t.Fatal("create did not assign an id")
	}

	byRestaurant, err := mgrs.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list by restaurant: %v", err)
	}
	if len(byRestaurant) != 1 || byRestaurant[0].UserID != uid || byRestaurant[0].Role != domain.StaffRoleHostess {
		t.Errorf("list by restaurant = %+v", byRestaurant)
	}

	byUser, err := mgrs.ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(byUser) != 1 || byUser[0].RestaurantID != rid {
		t.Errorf("list by user = %+v", byUser)
	}

	got, err := mgrs.GetByID(ctx, mn.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.RestaurantID != rid || got.UserID != uid || got.Role != domain.StaffRoleHostess {
		t.Errorf("get by id = %+v", got)
	}

	if _, err := mgrs.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get by id (missing) err = %v, want ErrNotFound", err)
	}

	if err := mgrs.UpdateRole(ctx, mn.ID, domain.StaffRoleManager); err != nil {
		t.Fatalf("update role: %v", err)
	}
	got, err = mgrs.GetByID(ctx, mn.ID)
	if err != nil {
		t.Fatalf("get by id after update: %v", err)
	}
	if got.Role != domain.StaffRoleManager {
		t.Errorf("role after update = %s, want manager", got.Role)
	}

	if err := mgrs.UpdateRole(ctx, uuid.New(), domain.StaffRoleManager); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update role (missing) err = %v, want ErrNotFound", err)
	}
}
