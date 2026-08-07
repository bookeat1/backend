package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// validInput is the minimal valid SaveInput (required enumerated fields set).
func validInput() SaveInput {
	return SaveInput{
		Name:          strp("Ok"),
		City:          strp(string(domain.CityAlmaty)),
		PriceCategory: strp(string(domain.PriceLow)),
	}
}

func TestCreateValidatesAndSavesCollections(t *testing.T) {
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	images := []domain.Image{{ImageURL: "a"}}
	in := validInput()
	in.Images = &images
	_, err := f.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.created == nil || repo.created.ID == uuid.Nil {
		t.Error("expected restaurant created with generated ID")
	}
	if !repo.created.IsActive {
		t.Error("expected new restaurant to default to active when IsActive is nil")
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
	in := validInput()
	in.Images = &images
	_, err := f.Update(context.Background(), id, in)
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

// TestUpdatePreservesUntouchedScalars proves the blocker #1 fix: a PATCH that
// carries only a new name must not blank/NULL every other scalar column. The
// stored row's description, kwaaka_restaurant_id and hidden_from_home have to
// survive read-modify-write.
func TestUpdatePreservesUntouchedScalars(t *testing.T) {
	id := uuid.New()
	kwaaka := "kw-123"
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID: id, Name: "Old", Description: "keep me", City: domain.CityAlmaty,
		PriceCategory: domain.PriceLow, KwaakaRestaurantID: &kwaaka, HiddenFromHome: true,
	}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{Name: strp("New name")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got == nil {
		t.Fatal("expected restaurant to be updated")
	}
	if got.Name != "New name" {
		t.Errorf("Name = %q, want the provided value", got.Name)
	}
	if got.Description != "keep me" {
		t.Errorf("Description = %q, want it preserved from the stored row", got.Description)
	}
	if got.KwaakaRestaurantID == nil || *got.KwaakaRestaurantID != kwaaka {
		t.Errorf("KwaakaRestaurantID = %v, want it preserved (not NULLed)", got.KwaakaRestaurantID)
	}
	if !got.HiddenFromHome {
		t.Error("HiddenFromHome = false, want it preserved as true")
	}
	if string(got.City) != string(domain.CityAlmaty) {
		t.Errorf("City = %q, want it preserved", got.City)
	}
}

// TestUpdatePreservesIsActiveWhenOmitted proves a PATCH that omits is_active
// (IsActive == nil) must not silently reactivate a soft-deleted restaurant.
func TestUpdatePreservesIsActiveWhenOmitted(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{ID: id, IsActive: false},
	}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	in := validInput()
	in.IsActive = nil
	_, err := f.Update(context.Background(), id, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated == nil {
		t.Fatal("expected restaurant to be updated")
	}
	if repo.updated.IsActive {
		t.Error("expected IsActive to remain false when IsActive is omitted, got true (silent reactivation)")
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

	in := validInput()
	in.IsActive = boolp(true)
	_, err := f.Update(context.Background(), id, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated == nil || !repo.updated.IsActive {
		t.Error("expected IsActive to be set to true when IsActive=&true")
	}
}

// TestUpdateOnlyReplacesProvidedCollections proves a PATCH that only carries
// images must not wipe features/tags/social_links.
func TestUpdateOnlyReplacesProvidedCollections(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	images := []domain.Image{{ImageURL: "a"}}
	in := validInput()
	in.Images = &images
	_, err := f.Update(context.Background(), id, in)
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

	_, err := f.Update(context.Background(), uuid.New(), validInput())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if rel.replaced != 0 {
		t.Error("expected no collection replace calls when the restaurant doesn't exist")
	}
}

func TestUpdateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	in := validInput()
	in.City = strp("Nowhere")
	_, err := f.Update(context.Background(), uuid.New(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestCreateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	in := validInput()
	in.City = strp("Nowhere")
	_, err := f.Create(context.Background(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestCreateRejectsMissingName(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	_, err := f.Create(context.Background(), SaveInput{
		City: strp(string(domain.CityAlmaty)), PriceCategory: strp(string(domain.PriceLow)),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation for missing name", err)
	}
}

func intp(i int) *int { return &i }

// TestCreateRejectsHalfSetPriceRange proves the both-or-neither rule: a range
// with only one bound provided is refused before any DB round-trip, mirroring
// migration 0068's CHECK. Two sub-cases: only price_min, only price_max.
func TestCreateRejectsHalfSetPriceRange(t *testing.T) {
	cases := map[string]SaveInput{
		"only min": {PriceMin: intp(8000)},
		"only max": {PriceMax: intp(15000)},
	}
	for name, prices := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
			in := validInput()
			in.PriceMin = prices.PriceMin
			in.PriceMax = prices.PriceMax
			_, err := f.Create(context.Background(), in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// TestCreateRejectsInvertedPriceRange rejects price_max < price_min.
func TestCreateRejectsInvertedPriceRange(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	in := validInput()
	in.PriceMin = intp(15000)
	in.PriceMax = intp(8000)
	_, err := f.Create(context.Background(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

// TestCreateRejectsNegativePriceRange rejects a negative lower bound.
func TestCreateRejectsNegativePriceRange(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
	in := validInput()
	in.PriceMin = intp(-1)
	in.PriceMax = intp(15000)
	_, err := f.Create(context.Background(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

// TestCreateAcceptsValidPriceRange accepts a normal range and an equal-bounds
// range, and threads both bounds onto the persisted row.
func TestCreateAcceptsValidPriceRange(t *testing.T) {
	cases := map[string]struct{ min, max int }{
		"normal": {8000, 15000},
		"equal":  {5000, 5000},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
			f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
			in := validInput()
			in.PriceMin = intp(want.min)
			in.PriceMax = intp(want.max)
			_, err := f.Create(context.Background(), in)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			got := repo.created
			if got == nil || got.PriceMin == nil || got.PriceMax == nil {
				t.Fatalf("expected persisted row with both bounds set, got %+v", got)
			}
			if *got.PriceMin != want.min || *got.PriceMax != want.max {
				t.Errorf("range = %d/%d, want %d/%d", *got.PriceMin, *got.PriceMax, want.min, want.max)
			}
		})
	}
}

// TestUpdateValidatesMergedPriceRange is the important one: validation runs on
// the FINAL merged row, not the raw PATCH input. The stored row already has a
// full range; a PATCH that carries only price_min is judged against the merged
// pair — accepted when the merge stays ordered, rejected when it inverts.
func TestUpdateValidatesMergedPriceRange(t *testing.T) {
	newRepo := func() *fakeRestaurantRepo {
		return &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
			Name: "Old", City: domain.CityAlmaty, PriceCategory: domain.PriceLow,
			PriceMin: intp(8000), PriceMax: intp(15000),
		}}}
	}

	t.Run("merged pair stays valid", func(t *testing.T) {
		repo := newRepo()
		f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
		// Only price_min provided; merged with stored max=15000 → 10000/15000 (valid).
		_, err := f.Update(context.Background(), uuid.New(), SaveInput{PriceMin: intp(10000)})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		got := repo.updated
		if got == nil || got.PriceMin == nil || got.PriceMax == nil {
			t.Fatalf("expected updated row with both bounds, got %+v", got)
		}
		if *got.PriceMin != 10000 || *got.PriceMax != 15000 {
			t.Errorf("merged range = %d/%d, want 10000/15000", *got.PriceMin, *got.PriceMax)
		}
	})

	t.Run("merged pair becomes inverted", func(t *testing.T) {
		repo := newRepo()
		f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})
		// Only price_min provided; merged with stored max=15000 → 20000/15000 (inverted).
		_, err := f.Update(context.Background(), uuid.New(), SaveInput{PriceMin: intp(20000)})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("err = %v, want ErrValidation", err)
		}
		if repo.updated != nil {
			t.Error("expected no Update call when the merged range is invalid")
		}
	})
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

// Manager/staff-role tests moved to managers_test.go (RBAC is a big enough
// surface to deserve its own file, separate from the catalog facade above).
