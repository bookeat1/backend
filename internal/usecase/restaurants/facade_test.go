package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestCreateValidatesAndSavesCollections(t *testing.T) {
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, inlineTx{})

	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		Images:     []domain.Image{{ImageURL: "a"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.created == nil || repo.created.ID == uuid.Nil {
		t.Error("expected restaurant created with generated ID")
	}
	if rel.replaced != 4 { // images, features, tags, social
		t.Errorf("replaced collections = %d, want 4", rel.replaced)
	}
}

func TestCreateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, inlineTx{})
	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Bad", City: "Nowhere", PriceCategory: domain.PriceLow},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestSubmitPartnershipValidates(t *testing.T) {
	p := &fakePartners{}
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, p, inlineTx{})
	if err := f.SubmitPartnership(context.Background(), PartnershipInput{}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("empty submit err = %v, want ErrValidation", err)
	}
	if err := f.SubmitPartnership(context.Background(), PartnershipInput{
		RestaurantName: "R", ContactName: "C", Email: "e@x.io", Phone: "+7700",
	}); err != nil {
		t.Fatalf("valid submit: %v", err)
	}
	if p.created == nil || p.created.Status != "pending" {
		t.Error("expected partnership request created with pending status")
	}
}

func TestManagerAssignChecksUserExists(t *testing.T) {
	u := NewManagerUseCase(&fakeManagers{}, &fakeUsers{err: domain.ErrNotFound})
	if _, err := u.Assign(context.Background(), AssignManagerInput{UserID: uuid.New(), RestaurantID: uuid.New()}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("assign missing user err = %v, want ErrNotFound", err)
	}
}

func TestManagerManages(t *testing.T) {
	rid := uuid.New()
	fm := &fakeManagers{byUser: []domain.RestaurantManager{{RestaurantID: rid}}}
	u := NewManagerUseCase(fm, &fakeUsers{})
	ok, err := u.Manages(context.Background(), uuid.New(), rid)
	if err != nil || !ok {
		t.Errorf("Manages = %v, %v; want true, nil", ok, err)
	}
	ok, _ = u.Manages(context.Background(), uuid.New(), uuid.New())
	if ok {
		t.Error("Manages = true for unrelated restaurant, want false")
	}
}
