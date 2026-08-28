package homepicks

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakePicks is the curation table: city key → ordered venue ids.
type fakePicks struct {
	lists map[string][]uuid.UUID
	err   error
	// listed records every city key ListIDs was asked for, in order. The
	// fallback chain is a sequence of lookups, and the tests assert on the
	// sequence, not only on the answer.
	listed  []string
	saved   []uuid.UUID
	savedTo string
}

func newFakePicks() *fakePicks { return &fakePicks{lists: map[string][]uuid.UUID{}} }

func (f *fakePicks) ListIDs(_ context.Context, city string) ([]uuid.UUID, error) {
	f.listed = append(f.listed, city)
	if f.err != nil {
		return nil, f.err
	}
	return f.lists[city], nil
}

func (f *fakePicks) Replace(_ context.Context, city string, ids []uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.savedTo, f.saved = city, ids
	f.lists[city] = ids
	return nil
}

// fakeCatalog stands in for usecase/restaurants.Facade. It answers the two
// filter shapes this package builds — "the popular ones" and "these ids" — and
// records every filter it was handed, because the whole safety claim of this
// feature is about WHICH query runs when nothing is curated.
type fakeCatalog struct {
	byID map[uuid.UUID]domain.RestaurantListItem
	// popular is the automatic rail's answer, in catalog order.
	popular []domain.RestaurantListItem
	filters []domain.RestaurantFilter
	err     error
}

func (f *fakeCatalog) List(_ context.Context, flt domain.RestaurantFilter, _ domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	f.filters = append(f.filters, flt)
	if f.err != nil {
		return nil, 0, f.err
	}
	if len(flt.IDs) == 0 {
		out := f.popular
		if flt.PerPage > 0 && len(out) > flt.PerPage {
			out = out[:flt.PerPage]
		}
		return out, len(f.popular), nil
	}
	// The real listing answers in CATALOG order and drops what does not match,
	// so this one walks its own store rather than the requested ids: a fake
	// that echoed the ids back in order would hide the bug where the facade
	// forgets to re-apply the editorial order.
	out := make([]domain.RestaurantListItem, 0, len(flt.IDs))
	for _, it := range f.byID {
		for _, want := range flt.IDs {
			if it.Restaurant.ID != want {
				continue
			}
			if !it.Restaurant.IsActive && !flt.IncludeInactive {
				break
			}
			out = append(out, it)
			break
		}
	}
	sortByName(out)
	return out, len(out), nil
}

func sortByName(items []domain.RestaurantListItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Restaurant.Name < items[j-1].Restaurant.Name; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func venue(name string, active bool) domain.RestaurantListItem {
	return domain.RestaurantListItem{
		Restaurant: domain.Restaurant{ID: uuid.New(), Name: name, IsActive: active},
	}
}

func catalogOf(items ...domain.RestaurantListItem) *fakeCatalog {
	c := &fakeCatalog{byID: map[uuid.UUID]domain.RestaurantListItem{}}
	for _, it := range items {
		c.byID[it.Restaurant.ID] = it
	}
	return c
}

func names(items []domain.RestaurantListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant.Name)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ids(items ...domain.RestaurantListItem) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant.ID)
	}
	return out
}

// THE guarantee of this whole change. Until somebody curates a list, every city
// must get the rail it got before migration 0090 — the popular venues in
// catalog order — and it must get it through the SAME query the app used to
// send: is_popular = true, no city filter, no id filter.
//
// If this test ever has to be relaxed, the main screen of every guest in every
// city changes on the same deploy.
func TestEmptyCurationKeepsTheOldAutomaticRail(t *testing.T) {
	a, b := venue("Первое по каталогу", true), venue("Второе по каталогу", true)
	catalog := catalogOf(a, b)
	catalog.popular = []domain.RestaurantListItem{a, b}
	f := NewFacade(newFakePicks(), catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Первое по каталогу", "Второе по каталогу"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
	if len(catalog.filters) != 1 {
		t.Fatalf("expected exactly one catalog read, got %d", len(catalog.filters))
	}
	flt := catalog.filters[0]
	if flt.IsPopular == nil || !*flt.IsPopular {
		t.Fatalf("automatic rail must ask for is_popular=true, got %+v", flt.IsPopular)
	}
	if flt.City != nil {
		t.Fatalf("automatic rail must NOT filter by city (that is today's behaviour), got %v", *flt.City)
	}
	if len(flt.IDs) != 0 {
		t.Fatalf("automatic rail must not filter by ids, got %v", flt.IDs)
	}
	if flt.IncludeInactive {
		t.Fatal("automatic rail must never include inactive venues")
	}
	if flt.PerPage != DefaultLimit {
		t.Fatalf("automatic rail page size = %d, want the default %d", flt.PerPage, DefaultLimit)
	}
}

// The fallback chain, asserted as a sequence: the city's own list is asked for
// first, the all-cities list second, and only then does the automatic rule run.
func TestCityWithoutItsOwnListFallsBackToTheAllCitiesOne(t *testing.T) {
	a, b := venue("Общая подборка", true), venue("Популярное", true)
	catalog := catalogOf(a, b)
	catalog.popular = []domain.RestaurantListItem{b}
	picks := newFakePicks()
	picks.lists[domain.HomePicksAllCities] = ids(a)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Астана", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Общая подборка"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
	if want := []string{"Астана", domain.HomePicksAllCities}; !equal(picks.listed, want) {
		t.Fatalf("lookup order:\n got %v\nwant %v", picks.listed, want)
	}
}

// A city that HAS its own list never sees the all-cities one: the two are
// alternatives, not layers to merge. Merging would make «убрать заведение из
// Алматы» impossible while it sits in the shared list.
func TestCityListWinsOverTheAllCitiesList(t *testing.T) {
	own, shared := venue("Алматинское", true), venue("Общее", true)
	catalog := catalogOf(own, shared)
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(own)
	picks.lists[domain.HomePicksAllCities] = ids(shared)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Алматинское"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
	if want := []string{"Алматы"}; !equal(picks.listed, want) {
		t.Fatalf("the all-cities list must not even be read, lookups: %v", picks.listed)
	}
}

// The point of the feature: the owner's order is the order on screen, even
// though the catalog would sort these venues the other way round.
func TestCuratedOrderSurvivesTheCatalogsOwnOrdering(t *testing.T) {
	// Alphabetically «Альфа» precedes «Омега», and the fake catalog sorts by
	// name exactly like the real listing sorts by display_order/name. The
	// curation says the opposite.
	alpha, omega := venue("Альфа", true), venue("Омега", true)
	catalog := catalogOf(alpha, omega)
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(omega, alpha)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Омега", "Альфа"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
}

// A venue switched off (or deleted — the row goes with it) must not break the
// rail: it drops out and the rest keeps its order. The curation itself is not
// touched, so the venue returns when it is switched back on.
func TestDeactivatedVenueDropsOutWithoutBreakingTheRail(t *testing.T) {
	first, dark, last := venue("Первое", true), venue("Погашенное", false), venue("Третье", true)
	catalog := catalogOf(first, dark, last)
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(first, dark, last)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Первое", "Третье"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
}

// An id nobody knows about any more (the venue was deleted between the read of
// the curation and the read of the catalog) is the same non-event.
func TestUnknownVenueIDIsIgnored(t *testing.T) {
	alive := venue("Живое", true)
	catalog := catalogOf(alive)
	picks := newFakePicks()
	picks.lists["Алматы"] = []uuid.UUID{uuid.New(), alive.Restaurant.ID}
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Живое"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
}

// A curation whose every venue has gone dark must not empty the main screen.
// It falls back exactly like no curation at all — the guest gets a rail, and
// the owner's list is still there when the venues come back.
func TestFullyDeactivatedCurationFallsBackToTheAutomaticRail(t *testing.T) {
	dark, popular := venue("Погашенное", false), venue("Популярное", true)
	catalog := catalogOf(dark, popular)
	catalog.popular = []domain.RestaurantListItem{popular}
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(dark)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"Популярное"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
}

func TestGuestRailIsCappedByTheLimit(t *testing.T) {
	a, b, c := venue("А", true), venue("Б", true), venue("В", true)
	catalog := catalogOf(a, b, c)
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(a, b, c)
	f := NewFacade(picks, catalog)

	got, err := f.Guest(context.Background(), "Алматы", 2)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"А", "Б"}; !equal(names(got), want) {
		t.Fatalf("rail:\n got %v\nwant %v", names(got), want)
	}
}

func TestGuestLimitIsClampedToMax(t *testing.T) {
	catalog := catalogOf()
	f := NewFacade(newFakePicks(), catalog)
	if _, err := f.Guest(context.Background(), "Алматы", 10_000); err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if got := catalog.filters[0].PerPage; got != MaxLimit {
		t.Fatalf("page size = %d, want the cap %d", got, MaxLimit)
	}
}

// The editor is asking "what did I pick", so an empty curation answers with an
// empty list — never with the automatic rail, which would read as "I have
// already curated this city" and is the one way to make somebody overwrite a
// rail they never built.
func TestEditorShowsNothingWhenNothingIsCurated(t *testing.T) {
	catalog := catalogOf(venue("Популярное", true))
	catalog.popular = []domain.RestaurantListItem{venue("Популярное", true)}
	f := NewFacade(newFakePicks(), catalog)

	got, err := f.Editor(context.Background(), "Алматы")
	if err != nil {
		t.Fatalf("editor list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("editor list = %v, want empty", names(got))
	}
	if len(catalog.filters) != 0 {
		t.Fatalf("editor must not fall back to the catalog, filters: %+v", catalog.filters)
	}
}

// The editor's list keeps a deactivated venue, and keeps it in its slot: they
// have to see that slot 2 of their rail is currently dark.
func TestEditorKeepsDeactivatedVenuesInPlace(t *testing.T) {
	first, dark := venue("Первое", true), venue("Погашенное", false)
	catalog := catalogOf(first, dark)
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(first, dark)
	f := NewFacade(picks, catalog)

	got, err := f.Editor(context.Background(), "Алматы")
	if err != nil {
		t.Fatalf("editor list: %v", err)
	}
	if want := []string{"Первое", "Погашенное"}; !equal(names(got), want) {
		t.Fatalf("editor list:\n got %v\nwant %v", names(got), want)
	}
	if !catalog.filters[0].IncludeInactive {
		t.Fatal("editor read must set IncludeInactive")
	}
}

func TestReplaceSavesTheGivenOrderUnderTheGivenCity(t *testing.T) {
	a, b := venue("А", true), venue("Б", true)
	picks := newFakePicks()
	f := NewFacade(picks, catalogOf(a, b))

	want := ids(b, a)
	if err := f.Replace(context.Background(), "Алматы", want); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if picks.savedTo != "Алматы" {
		t.Fatalf("saved under %q, want Алматы", picks.savedTo)
	}
	if len(picks.saved) != 2 || picks.saved[0] != want[0] || picks.saved[1] != want[1] {
		t.Fatalf("saved %v, want %v", picks.saved, want)
	}
}

// Clearing the list is a supported operation, not an error: it is how the owner
// hands the city back to the automatic rail.
func TestReplaceWithAnEmptyListClearsTheCuration(t *testing.T) {
	a := venue("А", true)
	catalog := catalogOf(a)
	catalog.popular = []domain.RestaurantListItem{a}
	picks := newFakePicks()
	picks.lists["Алматы"] = ids(a)
	f := NewFacade(picks, catalog)

	if err := f.Replace(context.Background(), "Алматы", nil); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("guest rail: %v", err)
	}
	if want := []string{"А"}; !equal(names(got), want) {
		t.Fatalf("rail after clearing:\n got %v\nwant %v", names(got), want)
	}
	// And it went back through the automatic rule, not through a stale curation.
	last := catalog.filters[len(catalog.filters)-1]
	if last.IsPopular == nil || !*last.IsPopular {
		t.Fatalf("after clearing the rail must be automatic again, got %+v", last)
	}
}

// A list naming the same venue twice is refused rather than de-duplicated: it
// means the panel and the server disagree about the rail, and guessing which
// slot was meant is guessing.
func TestReplaceRefusesDuplicates(t *testing.T) {
	a := venue("А", true)
	picks := newFakePicks()
	f := NewFacade(picks, catalogOf(a))

	err := f.Replace(context.Background(), "Алматы", []uuid.UUID{a.Restaurant.ID, a.Restaurant.ID})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want a validation failure", err)
	}
	if picks.saved != nil {
		t.Fatalf("nothing must be written on a refused save, got %v", picks.saved)
	}
}

func TestReplaceRefusesMoreThanTheCap(t *testing.T) {
	tooMany := make([]uuid.UUID, MaxLimit+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	picks := newFakePicks()
	f := NewFacade(picks, catalogOf())

	if err := f.Replace(context.Background(), "Алматы", tooMany); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want a validation failure", err)
	}
}

// A read failure is reported, never swallowed into "nothing is curated" — that
// would silently replace a curated rail with the automatic one during a
// database blip, and nobody would see it happen.
func TestGuestPropagatesACurationReadFailure(t *testing.T) {
	picks := newFakePicks()
	picks.err = errors.New("boom")
	catalog := catalogOf()
	f := NewFacade(picks, catalog)

	if _, err := f.Guest(context.Background(), "Алматы", 0); err == nil {
		t.Fatal("expected the read failure to surface")
	}
	if len(catalog.filters) != 0 {
		t.Fatal("a failed curation read must not fall through to the automatic rail")
	}
}

// fakeCities stands in for usecase/cities: it folds a written spelling to the
// dictionary entry whose Name is what actually sits in restaurants.city.
type fakeCities struct {
	bySpelling map[string]string // written spelling → canonical name
	asked      []string
	err        error
}

func (f *fakeCities) Resolve(_ context.Context, raw string) (*domain.CityEntry, error) {
	f.asked = append(f.asked, raw)
	if f.err != nil {
		return nil, f.err
	}
	name, ok := f.bySpelling[raw]
	if !ok {
		// The dictionary's own answer for "no such city": no entry, no error.
		return nil, nil
	}
	return &domain.CityEntry{Name: name}, nil
}

// The reason the resolver is wired in at all: the panel saves the canonical
// name («Астана») while a phone may still be sending a code or an older
// spelling («astana», «Нур-Султан»). If those are not folded to one key, the
// owner curates a rail and the guests it was curated for never see it.
func TestGuestFindsACurationSavedUnderTheCanonicalCityName(t *testing.T) {
	picks := newFakePicks()
	a, b := venue("A", true), venue("B", true)
	picks.lists["Астана"] = ids(b, a)
	cat := catalogOf(a, b)
	f := NewFacade(picks, cat, WithCityResolver(&fakeCities{
		bySpelling: map[string]string{"astana": "Астана", "Нур-Султан": "Астана"},
	}))

	for _, spelling := range []string{"astana", "Нур-Султан", "Астана"} {
		got, err := f.Guest(context.Background(), spelling, 0)
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if !equal(names(got), []string{"B", "A"}) {
			t.Fatalf("%s: curated rail not found, got %v", spelling, names(got))
		}
	}
	if len(cat.filters) != 3 {
		t.Fatalf("expected one catalog read per call, got %d", len(cat.filters))
	}
	for i, flt := range cat.filters {
		if len(flt.IDs) == 0 {
			t.Fatalf("call %d fell through to the automatic rail", i)
		}
	}
}

// The editor and the save must land on the SAME key the guest read uses, or an
// owner typing a synonym would create a second, invisible rail.
func TestEditorAndReplaceUseTheCanonicalCityKey(t *testing.T) {
	picks := newFakePicks()
	a := venue("A", true)
	f := NewFacade(picks, catalogOf(a), WithCityResolver(&fakeCities{
		bySpelling: map[string]string{"astana": "Астана"},
	}))

	if err := f.Replace(context.Background(), "astana", ids(a)); err != nil {
		t.Fatal(err)
	}
	if picks.savedTo != "Астана" {
		t.Fatalf("saved under %q, want the canonical %q", picks.savedTo, "Астана")
	}
	got, err := f.Editor(context.Background(), "astana")
	if err != nil {
		t.Fatal(err)
	}
	if !equal(names(got), []string{"A"}) {
		t.Fatalf("editor read a different key: %v", names(got))
	}
}

// The all-cities key is the ABSENCE of a city, not a city name — resolving it
// would hand the dictionary an empty string and, if anything ever came back,
// silently move the default rail under a real city.
func TestTheAllCitiesKeyIsNeverResolved(t *testing.T) {
	picks := newFakePicks()
	a := venue("A", true)
	picks.lists[domain.HomePicksAllCities] = ids(a)
	cities := &fakeCities{bySpelling: map[string]string{"": "Алматы"}}
	f := NewFacade(picks, catalogOf(a), WithCityResolver(cities))

	got, err := f.Guest(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(names(got), []string{"A"}) {
		t.Fatalf("the all-cities rail was lost: %v", names(got))
	}
	if len(cities.asked) != 0 {
		t.Fatalf("the dictionary was asked about the all-cities key: %v", cities.asked)
	}
}

// A dictionary outage must not empty the main screen: the raw spelling is what
// the panel most likely stored, so it is the safe fallback.
func TestADictionaryFailureFallsBackToTheRawCityKey(t *testing.T) {
	picks := newFakePicks()
	a := venue("A", true)
	picks.lists["Алматы"] = ids(a)
	f := NewFacade(picks, catalogOf(a), WithCityResolver(&fakeCities{err: errors.New("dictionary down")}))

	got, err := f.Guest(context.Background(), "Алматы", 0)
	if err != nil {
		t.Fatalf("a dictionary blip broke the rail: %v", err)
	}
	if !equal(names(got), []string{"A"}) {
		t.Fatalf("curation lost on a dictionary blip: %v", names(got))
	}
}

// An unknown city is not an error and not a 422: it simply has no rail of its
// own and falls through the ordinary chain to the all-cities list.
func TestAnUnknownCityFallsThroughToTheAllCitiesRail(t *testing.T) {
	picks := newFakePicks()
	a := venue("A", true)
	picks.lists[domain.HomePicksAllCities] = ids(a)
	f := NewFacade(picks, catalogOf(a), WithCityResolver(&fakeCities{bySpelling: map[string]string{}}))

	got, err := f.Guest(context.Background(), "Атлантида", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(names(got), []string{"A"}) {
		t.Fatalf("unknown city did not reach the all-cities rail: %v", names(got))
	}
}
