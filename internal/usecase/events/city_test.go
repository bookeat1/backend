package events

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeCities is the city dictionary as this package sees it: a table of written
// spellings (already folded to the lookup key) pointing at dictionary entries.
// It records what it was asked so a test can prove the resolver was consulted
// at all, not merely that the answer happened to match.
type fakeCities struct {
	byAlias map[string]*domain.CityEntry
	asked   []string
	err     error
}

func (f *fakeCities) Resolve(_ context.Context, raw string) (*domain.CityEntry, error) {
	f.asked = append(f.asked, raw)
	if f.err != nil {
		return nil, f.err
	}
	return f.byAlias[domain.NormalizeCityKey(raw)], nil
}

// dictionary mirrors the two entries migration 0081 seeds, with the aliases it
// registers for them: the code, the Russian name, and the historical spelling.
func dictionary() *fakeCities {
	almaty := &domain.CityEntry{ID: uuid.New(), Code: "almaty", Name: "Алматы", IsActive: true}
	astana := &domain.CityEntry{ID: uuid.New(), Code: "astana", Name: "Астана", IsActive: true}
	return &fakeCities{byAlias: map[string]*domain.CityEntry{
		"almaty":     almaty,
		"алматы":     almaty,
		"alma-ata":   almaty,
		"алма-ата":   almaty,
		"astana":     astana,
		"астана":     astana,
		"нур-султан": astana,
	}}
}

func cityPtr(v string) *domain.City {
	c := domain.City(v)
	return &c
}

// cityOrEmpty renders an optional city for a failure message: a bare %v on the
// pointer would print an address, which tells a reader nothing about what went
// wrong.
func cityOrEmpty(c *domain.City) string {
	if c == nil {
		return ""
	}
	return string(*c)
}

// TestListPublicUpcoming_CityFilterIsResolvedThroughTheDictionary is the whole
// point of wiring the resolver here: the Афиша compares cities as exact
// strings, so a client that sends the CODE (a new build), a historical name (a
// stale build) or a different case must all end up filtering by the one
// spelling that is actually stored. Before this, only the exact Russian name
// worked and everything else matched zero rows in silence.
func TestListPublicUpcoming_CityFilterIsResolvedThroughTheDictionary(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent string
		want domain.City
	}{
		{"code", "almaty", "Алматы"},
		{"historical name", "alma-ata", "Алматы"},
		{"lower case russian", "алматы", "Алматы"},
		{"exact stored spelling", "Астана", "Астана"},
		{"renamed city's previous name", "Нур-Султан", "Астана"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(dictionary()))

			if _, _, err := f.ListPublicUpcoming(context.Background(),
				domain.PublicEventFilter{City: cityPtr(tc.sent)}); err != nil {
				t.Fatalf("ListPublicUpcoming: %v", err)
			}
			got := repo.publicFilter.City
			if got == nil || *got != tc.want {
				t.Fatalf("store was asked for city %q, want %q", cityOrEmpty(got), tc.want)
			}
		})
	}
}

// TestListPublicUpcoming_UnknownCityIsPassedThroughUntouched: a value the
// dictionary does not know must behave exactly as it did before the dictionary
// existed — compared as written, matching nothing. Turning it into an error
// would break every client that ever sent a typo.
func TestListPublicUpcoming_UnknownCityIsPassedThroughUntouched(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(dictionary()))

	if _, _, err := f.ListPublicUpcoming(context.Background(),
		domain.PublicEventFilter{City: cityPtr("Атлантида")}); err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if got := repo.publicFilter.City; got == nil || *got != "Атлантида" {
		t.Fatalf("store was asked for city %q, want the raw value back", cityOrEmpty(got))
	}
}

// TestListPublicUpcoming_DictionaryOutageStillServesTheListing: a broken
// dictionary must degrade the filter, not the screen. The listing falls back to
// the raw value (pre-dictionary behaviour) instead of answering 500.
func TestListPublicUpcoming_DictionaryOutageStillServesTheListing(t *testing.T) {
	repo := newFakeRepo()
	broken := &fakeCities{err: errors.New("dictionary down")}
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(broken))

	if _, _, err := f.ListPublicUpcoming(context.Background(),
		domain.PublicEventFilter{City: cityPtr("almaty")}); err != nil {
		t.Fatalf("a dictionary outage must not fail the listing, got %v", err)
	}
	if repo.publicCalls != 1 {
		t.Fatalf("the store was queried %d times, want 1", repo.publicCalls)
	}
	if got := repo.publicFilter.City; got == nil || *got != "almaty" {
		t.Fatalf("store was asked for city %q, want the raw value back", cityOrEmpty(got))
	}
}

// TestListPublicUpcoming_NoResolverKeepsThePreDictionaryBehaviour guards the
// unwired path: a service started without the dictionary must still answer,
// comparing the raw string exactly as it did before migration 0084.
func TestListPublicUpcoming_NoResolverKeepsThePreDictionaryBehaviour(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	if _, _, err := f.ListPublicUpcoming(context.Background(),
		domain.PublicEventFilter{City: cityPtr("almaty")}); err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if got := repo.publicFilter.City; got == nil || *got != "almaty" {
		t.Fatalf("store was asked for city %q, want the raw value back", cityOrEmpty(got))
	}
}

// --- the write side: the event's own city override ---

func TestCreate_CityOverrideIsStoredInTheDictionarySpelling(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cities := dictionary()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{},
		WithCityResolver(cities))

	in := validCreate(rid)
	raw := "alma-ata"
	in.City = &raw

	e, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if e.City == nil || *e.City != "Алматы" {
		t.Fatalf("stored city = %v, want the dictionary spelling «Алматы»", e.City)
	}
}

// TestCreate_NoCityMeansTheEventFollowsItsVenue: the normal case must stay
// empty rather than be filled in with the venue's city at write time — that
// copy is exactly what would go stale when the venue moves.
func TestCreate_NoCityMeansTheEventFollowsItsVenue(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{},
		WithCityResolver(dictionary()))

	for _, tc := range []struct {
		name string
		city *string
	}{
		{"absent", nil},
		{"blank", func() *string { s := "   "; return &s }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validCreate(rid)
			in.City = tc.city
			e, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if e.City != nil {
				t.Fatalf("city = %v, want no override at all", *e.City)
			}
		})
	}
}

// TestCreate_UnknownOrHiddenCityIsRefused: on the WRITE side an unrecognized
// city is a mistake to report. Storing it would produce an event that matches
// no filter at all and looks published to the venue that created it.
func TestCreate_UnknownOrHiddenCityIsRefused(t *testing.T) {
	hidden := &domain.CityEntry{ID: uuid.New(), Code: "shymkent", Name: "Шымкент", IsActive: false}
	cities := dictionary()
	cities.byAlias["шымкент"] = hidden

	for _, tc := range []struct{ name, city string }{
		{"unknown", "Атлантида"},
		{"hidden", "Шымкент"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rid, actorID := uuid.New(), uuid.New()
			repo := newFakeRepo()
			f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{},
				WithCityResolver(cities))

			in := validCreate(rid)
			in.City = &tc.city
			_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if repo.created != nil {
				t.Fatal("nothing must be written when the city is refused")
			}
		})
	}
}

// TestUpdate_ReSavingTheSameCityIsNotAContentEdit: the override is moderated
// content, so changing it demotes the card from the main screen — but re-saving
// the SAME city written differently («almaty» over a stored «Алматы») is not a
// change, and must not cost the venue a re-review.
func TestUpdate_ReSavingTheSameCityIsNotAContentEdit(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), feed,
		WithCityResolver(dictionary()))

	in := validCreate(rid)
	stored := "Алматы"
	in.City = &stored
	e, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	same := "almaty"
	upd := UpdateInput{
		Title: e.Title, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		Status: e.Status, City: &same,
	}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("the card was demoted %d times for an unchanged city", len(feed.demoted))
	}

	// ...while an actual move to another city IS an edit.
	moved := "Астана"
	upd.City = &moved
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(feed.demoted) != 1 {
		t.Fatalf("moving the event to another city must demote the card once, got %d", len(feed.demoted))
	}
}
