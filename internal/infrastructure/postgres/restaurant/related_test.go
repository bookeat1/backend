package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

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
