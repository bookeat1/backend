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
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	images := []domain.Image{{ImageURL: "a"}}
	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		Images:     &images,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.created == nil || repo.created.ID == uuid.Nil {
		t.Error("expected restaurant created with generated ID")
	}
	if !repo.created.IsActive {
		t.Error("expected new restaurant to default to active when SetActive is nil")
	}
	if rel.replaced != 4 { // images, features, tags, social
		t.Errorf("replaced collections = %d, want 4", rel.replaced)
	}
	if !rel.imagesReplaced || !rel.featuresReplaced || !rel.tagsReplaced || !rel.socialLinksReplaced {
		t.Error("expected Create to replace all four collections, including empty ones")
	}
}

func TestUpdateValidatesAndSavesCollections(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
	rel := &fakeRelated{}
	tx := &inlineTx{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, tx)

	images := []domain.Image{{ImageURL: "a"}}
	_, err := f.Update(context.Background(), id, SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		Images:     &images,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated == nil || repo.updated.ID != id {
		t.Error("expected restaurant updated with the passed id")
	}
	if !rel.imagesReplaced {
		t.Error("expected ReplaceImages to be called since Images was provided")
	}
	if !tx.called {
		t.Error("expected Update to route through the TxManager")
	}
}

// TestUpdatePreservesIsActiveWhenOmitted proves fix #2(a): a PATCH that omits
// is_active (SetActive == nil) must not silently reactivate a soft-deleted
// restaurant.
func TestUpdatePreservesIsActiveWhenOmitted(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{ID: id, IsActive: false},
	}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		SetActive:  nil,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated == nil {
		t.Fatal("expected restaurant to be updated")
	}
	if repo.updated.IsActive {
		t.Error("expected IsActive to remain false when SetActive is omitted, got true (silent reactivation)")
	}
}

// TestUpdateSetsActiveWhenProvided proves that an explicit is_active in the
// PATCH body still takes effect.
func TestUpdateSetsActiveWhenProvided(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{ID: id, IsActive: false},
	}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	active := true
	_, err := f.Update(context.Background(), id, SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		SetActive:  &active,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated == nil || !repo.updated.IsActive {
		t.Error("expected IsActive to be set to true when SetActive=&true")
	}
}

// TestUpdateOnlyReplacesProvidedCollections proves fix #2(b): a PATCH that
// only carries images must not wipe features/tags/social_links.
func TestUpdateOnlyReplacesProvidedCollections(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	images := []domain.Image{{ImageURL: "a"}}
	_, err := f.Update(context.Background(), id, SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		Images:     &images,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !rel.imagesReplaced {
		t.Error("expected ReplaceImages to be called since Images was provided")
	}
	if rel.featuresReplaced || rel.tagsReplaced || rel.socialLinksReplaced {
		t.Error("expected features/tags/social_links to be left untouched when omitted from the request")
	}
	if rel.replaced != 1 {
		t.Errorf("replaced collections = %d, want 1 (only images)", rel.replaced)
	}
}

// TestUpdatePropagatesNotFound proves that Updating a nonexistent restaurant
// surfaces ErrNotFound (so the handler maps it to 404) instead of blowing up
// or silently creating rows.
func TestUpdatePropagatesNotFound(t *testing.T) {
	repo := &fakeRestaurantRepo{getErr: domain.ErrNotFound}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), uuid.New(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if rel.replaced != 0 {
		t.Error("expected no collection replace calls when the restaurant doesn't exist")
	}
}

func TestUpdateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	_, err := f.Update(context.Background(), uuid.New(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Bad", City: "Nowhere", PriceCategory: domain.PriceLow},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestCreateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Bad", City: "Nowhere", PriceCategory: domain.PriceLow},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestSubmitPartnershipValidates(t *testing.T) {
	p := &fakePartners{}
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, p, &inlineTx{})
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

func TestManagerAssignSuccess(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	fm := &fakeManagers{}
	u := NewManagerUseCase(fm, &fakeUsers{})

	m, err := u.Assign(context.Background(), AssignManagerInput{
		RestaurantID: rid, UserID: uid, WhatsappOptIn: true,
	})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if fm.created == nil {
		t.Fatal("expected manager created")
	}
	if fm.created.RestaurantID != rid || fm.created.UserID != uid || !fm.created.WhatsappOptIn {
		t.Errorf("created = %+v, want RestaurantID=%v UserID=%v WhatsappOptIn=true", fm.created, rid, uid)
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
