package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeCityResolver is the city dictionary as this package sees it: a lookup
// from any registered spelling to one entry.
type fakeCityResolver struct {
	byKey map[string]domain.CityEntry
	err   error
	calls int
}

func newCityResolver() *fakeCityResolver {
	almaty := domain.CityEntry{ID: uuid.New(), Code: "almaty", Name: "Алматы", IsActive: true}
	hidden := domain.CityEntry{ID: uuid.New(), Code: "shymkent", Name: "Шымкент", IsActive: false}
	r := &fakeCityResolver{byKey: map[string]domain.CityEntry{}}
	for _, c := range []domain.CityEntry{almaty, hidden} {
		r.byKey[domain.NormalizeCityKey(c.Name)] = c
		r.byKey[domain.NormalizeCityKey(c.Code)] = c
	}
	// The city's previous name, kept resolvable exactly as a rename would keep
	// it — an old build may still be sending it.
	r.byKey["алма-ата"] = almaty
	return r
}

func (f *fakeCityResolver) Resolve(_ context.Context, raw string) (*domain.CityEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.byKey[domain.NormalizeCityKey(raw)]
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func facadeWithCities(repo *fakeRestaurantRepo, res cityResolver) Facade {
	return NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithCityResolver(res))
}

// TestCatalogFilterUnderstandsCodesAndOldSpellings is the compatibility promise
// of the whole feature: one server answers three generations of client. The
// filter that actually runs is a string comparison against the stored
// restaurants.city column, so every accepted spelling has to arrive at the
// repository as that ONE stored value.
func TestCatalogFilterUnderstandsCodesAndOldSpellings(t *testing.T) {
	for _, sent := range []string{"Алматы", "алматы", "almaty", "  Алма-Ата "} {
		repo := &fakeRestaurantRepo{}
		f := facadeWithCities(repo, newCityResolver())

		city := domain.City(sent)
		if _, _, err := f.List(context.Background(),
			domain.RestaurantFilter{City: &city}, domain.VenueStateFilter{}); err != nil {
			t.Fatalf("List(city=%q) = %v", sent, err)
		}
		if repo.lastList.City == nil {
			t.Fatalf("city=%q reached the repository as nil", sent)
		}
		if got := string(*repo.lastList.City); got != "Алматы" {
			t.Errorf("city=%q reached the repository as %q, want %q", sent, got, "Алматы")
		}

		repo2 := &fakeRestaurantRepo{}
		f2 := facadeWithCities(repo2, newCityResolver())
		c2 := domain.City(sent)
		if _, _, err := f2.Search(context.Background(),
			domain.RestaurantSearchFilter{City: &c2}, domain.VenueStateFilter{}); err != nil {
			t.Fatalf("Search(city=%q) = %v", sent, err)
		}
		if repo2.lastSearch.City == nil || string(*repo2.lastSearch.City) != "Алматы" {
			t.Errorf("search city=%q reached the repository as %v, want %q", sent, repo2.lastSearch.City, "Алматы")
		}
	}
}

// TestUnknownOrUnresolvableCityFallsBackToTheRawValue: an unknown city and a
// broken dictionary must both leave the catalog exactly as it behaved before
// the dictionary existed — the raw string goes to the query and matches
// nothing. Turning either into an error would take a browsable catalog down.
func TestUnknownOrUnresolvableCityFallsBackToTheRawValue(t *testing.T) {
	repo := &fakeRestaurantRepo{}
	f := facadeWithCities(repo, newCityResolver())
	unknown := domain.City("Караганда")
	if _, _, err := f.List(context.Background(),
		domain.RestaurantFilter{City: &unknown}, domain.VenueStateFilter{}); err != nil {
		t.Fatalf("List = %v", err)
	}
	if repo.lastList.City == nil || string(*repo.lastList.City) != "Караганда" {
		t.Errorf("unknown city reached the repository as %v, want the raw value", repo.lastList.City)
	}

	broken := newCityResolver()
	broken.err = errors.New("dictionary is down")
	repo2 := &fakeRestaurantRepo{}
	f2 := facadeWithCities(repo2, broken)
	almaty := domain.City("almaty")
	if _, _, err := f2.List(context.Background(),
		domain.RestaurantFilter{City: &almaty}, domain.VenueStateFilter{}); err != nil {
		t.Fatalf("List with a broken dictionary = %v, want the catalog to still answer", err)
	}
	if repo2.lastList.City == nil || string(*repo2.lastList.City) != "almaty" {
		t.Errorf("with a broken dictionary the filter became %v, want the raw value", repo2.lastList.City)
	}
}

// TestNoCityFilterDoesNotTouchTheDictionary: the overwhelming majority of
// catalog requests carry no city at all, and they must not pay for a lookup.
func TestNoCityFilterDoesNotTouchTheDictionary(t *testing.T) {
	res := newCityResolver()
	repo := &fakeRestaurantRepo{}
	f := facadeWithCities(repo, res)

	if _, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{}); err != nil {
		t.Fatalf("List = %v", err)
	}
	blank := domain.City("   ")
	if _, _, err := f.List(context.Background(),
		domain.RestaurantFilter{City: &blank}, domain.VenueStateFilter{}); err != nil {
		t.Fatalf("List = %v", err)
	}
	if res.calls != 0 {
		t.Errorf("the dictionary was queried %d times for requests without a city", res.calls)
	}
}

// TestVenueCityIsValidatedAgainstTheDictionary: before the dictionary the only
// guard was two constants compiled into the binary, so a third city could not
// be added without a release. Now the dictionary decides — and a HIDDEN city
// cannot be newly assigned, or hiding one would not stop it spreading.
func TestVenueCityIsValidatedAgainstTheDictionary(t *testing.T) {
	ctx := context.Background()

	repo := &fakeRestaurantRepo{}
	f := facadeWithCities(repo, newCityResolver())
	in := validInput()
	in.City = strp("almaty")
	if _, err := f.Create(ctx, in); err != nil {
		t.Fatalf("Create with a dictionary code = %v", err)
	}
	// The stored value is the dictionary's own spelling, not the caller's: the
	// database trigger would canonicalize it anyway, and echoing back what was
	// actually saved beats echoing what was sent.
	if repo.created == nil || string(repo.created.City) != "Алматы" {
		t.Errorf("stored city = %v, want %q", repo.created, "Алматы")
	}

	repo2 := &fakeRestaurantRepo{}
	f2 := facadeWithCities(repo2, newCityResolver())
	in2 := validInput()
	in2.City = strp("Караганда")
	if _, err := f2.Create(ctx, in2); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Create with a city outside the dictionary = %v, want ErrValidation", err)
	}
	if repo2.created != nil {
		t.Error("a venue with an unknown city was written")
	}

	repo3 := &fakeRestaurantRepo{}
	f3 := facadeWithCities(repo3, newCityResolver())
	in3 := validInput()
	in3.City = strp("Шымкент")
	if _, err := f3.Create(ctx, in3); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Create with a HIDDEN city = %v, want ErrValidation", err)
	}
	if repo3.created != nil {
		t.Error("a venue was assigned a hidden city")
	}
}
