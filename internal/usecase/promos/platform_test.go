package promos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// platformCreate is validCreate minus the venue — the one nil that makes the
// offer the platform's own.
func platformCreate() CreateInput {
	return CreateInput{
		Title:    "Городская акция",
		StartsAt: time.Now().Add(-time.Hour),
		EndsAt:   time.Now().Add(time.Hour),
	}
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

// fakeCities is the city dictionary this usecase resolves overrides through.
// It knows one city and treats everything else as unknown, which is exactly the
// answer the strict write path must refuse.
type fakeCities struct {
	entries map[string]*domain.CityEntry
	err     error
}

func (f *fakeCities) Resolve(_ context.Context, raw string) (*domain.CityEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries[domain.NormalizeCityKey(raw)], nil
}

func almatyDictionary() *fakeCities {
	return &fakeCities{entries: map[string]*domain.CityEntry{
		"almaty":    {Code: "almaty", Name: string(domain.CityAlmaty), IsActive: true},
		"алматы":    {Code: "almaty", Name: string(domain.CityAlmaty), IsActive: true},
		"алма-ата":  {Code: "almaty", Name: string(domain.CityAlmaty), IsActive: true},
		"караганда": {Code: "karaganda", Name: "Караганда", IsActive: false},
	}}
}

func TestCreate_PlatformPromoHasNoVenue(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{err: errors.New("must not be asked about a venue that does not exist")}, &fakeFeed{})

	p, err := f.Create(context.Background(), superadmin(), platformCreate())
	if err != nil {
		t.Fatalf("superadmin must be able to create a platform promo: %v", err)
	}
	if p.RestaurantID != nil {
		t.Fatalf("restaurant_id = %v, want nil", p.RestaurantID)
	}
	if !p.IsPlatform() {
		t.Fatal("IsPlatform must report true for a venue-less promo")
	}
}

func TestCreate_PlatformPromoRefusesAVenueRole(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, platformCreate())
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if repo.created != nil {
		t.Fatal("a refused create must write nothing")
	}
}

// The venue's own promos are untouched by the new branch.
func TestCreate_VenueBoundPromoIsUnaffected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	p, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if err != nil {
		t.Fatalf("a venue owner must still create their own promo: %v", err)
	}
	if p.RestaurantID == nil || *p.RestaurantID != rid {
		t.Fatalf("restaurant_id = %v, want %s", p.RestaurantID, rid)
	}
	if p.City != nil {
		t.Fatalf("city = %v, want nil: a promo with no override follows its venue", p.City)
	}
}

// A promo's city override is the same shape events got in 0084: written in any
// registered spelling, STORED in the dictionary's own.
func TestCreate_CityOverrideIsResolvedThroughTheDictionary(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(almatyDictionary()))

	for _, written := range []string{"almaty", "Алма-Ата", "  алматы  "} {
		in := platformCreate()
		in.City = &written
		p, err := f.Create(context.Background(), superadmin(), in)
		if err != nil {
			t.Fatalf("city %q must be accepted: %v", written, err)
		}
		if p.City == nil || *p.City != domain.CityAlmaty {
			t.Fatalf("city = %v, want the dictionary spelling %q", p.City, domain.CityAlmaty)
		}
	}
}

// An unknown or hidden city is a mistake to report, not a value to store and
// then never match — the same strictness usecase/events applies.
func TestCreate_CityOverrideRefusesUnknownAndHidden(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(almatyDictionary()))

	for _, written := range []string{"Атлантида", "Караганда"} {
		in := platformCreate()
		in.City = &written
		if _, err := f.Create(context.Background(), superadmin(), in); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("city %q: err = %v, want ErrValidation", written, err)
		}
	}
}

// Changing which city an approved card reaches is a content edit: the same
// approved words are handed to a different audience. Asserted on a VENUE promo
// — the demotion rule is about content the platform reviewed for somebody else.
func TestUpdate_ChangingTheCityDemotesTheCard(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed, WithCityResolver(almatyDictionary()))

	p, err := f.Create(context.Background(), superadmin(), validCreate(uuid.New()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	almaty := "almaty"
	if _, err := f.Update(context.Background(), superadmin(), p.ID, UpdateInput{
		Title: p.Title, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: domain.PromoPublished, City: &almaty,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 1 {
		t.Fatalf("demoted = %v, want the edited promo once", feed.demoted)
	}

	// Re-saving the same city in another spelling is NOT an edit: the resolved
	// value is what counts.
	spelled := "Алма-Ата"
	if _, err := f.Update(context.Background(), superadmin(), p.ID, UpdateInput{
		Title: p.Title, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: domain.PromoPublished, City: &spelled,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 1 {
		t.Fatalf("demoted = %v, want no second demotion for the same city", feed.demoted)
	}
}

func TestListPlatformAdmin(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	if _, err := f.Create(context.Background(), superadmin(), platformCreate()); err != nil {
		t.Fatalf("seed platform promo: %v", err)
	}
	if _, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid)); err != nil {
		t.Fatalf("seed venue promo: %v", err)
	}

	items, total, err := f.ListPlatformAdmin(context.Background(), superadmin(), nil, 1, 20)
	if err != nil {
		t.Fatalf("ListPlatformAdmin: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].RestaurantID != nil {
		t.Fatalf("got %d items %+v, want exactly the venue-less one", total, items)
	}

	if _, _, err := f.ListPlatformAdmin(context.Background(),
		Actor{UserID: actorID, Role: domain.RoleRestaurant}, nil, 1, 20); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden for a venue role", err)
	}
}

// The guest listing resolves ?city= through the dictionary before it reaches
// the store, so three generations of client all filter on one spelling.
func TestListPublicActive_CanonicalizesTheCityFilter(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithCityResolver(almatyDictionary()))

	raw := domain.City("almaty")
	if _, _, err := f.ListPublicActive(context.Background(), domain.PublicPromoFilter{City: &raw}); err != nil {
		t.Fatalf("ListPublicActive: %v", err)
	}
	if repo.publicFilter.City == nil || *repo.publicFilter.City != domain.CityAlmaty {
		t.Fatalf("store was asked for city %v, want %q", repo.publicFilter.City, domain.CityAlmaty)
	}

	// An unknown city is passed through untouched and simply matches nothing —
	// never a 500 and never a silently widened listing.
	unknown := domain.City("Атлантида")
	if _, _, err := f.ListPublicActive(context.Background(), domain.PublicPromoFilter{City: &unknown}); err != nil {
		t.Fatalf("an unknown city must not fail the request: %v", err)
	}
	if repo.publicFilter.City == nil || *repo.publicFilter.City != unknown {
		t.Fatalf("store was asked for %v, want the raw value passed through", repo.publicFilter.City)
	}
}

// --- creation-time approval of the platform's own content ---

// The bug this fixes: a platform promo was born at feed_status='not_submitted'
// and waited for a moderator who is the same person who wrote it. The platform
// approves its own content at creation, and the approval is recorded with the
// superadmin who made it.
func TestCreate_PlatformPromoIsApprovedForTheHomeFeedAtCreation(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	actor := superadmin()
	before := time.Now()
	p, err := f.Create(context.Background(), actor, platformCreate())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(feed.approvals) != 1 {
		t.Fatalf("approvals = %+v, want the platform promo approved exactly once", feed.approvals)
	}
	got := feed.approvals[0]
	if got.kind != domain.FeedItemPromo || got.itemID != p.ID {
		t.Fatalf("approved %s %s, want promo %s", got.kind, got.itemID, p.ID)
	}
	// Who approved it is the audit trail's whole point: it must be the actor,
	// not a nil uuid and not the item's own id.
	if got.reviewer != actor.UserID {
		t.Fatalf("reviewer = %s, want the creating superadmin %s", got.reviewer, actor.UserID)
	}
	if got.at.Before(before) || got.at.After(time.Now()) {
		t.Fatalf("approved at %v, want the creation instant", got.at)
	}
}

// Characterisation: venue moderation is untouched. A venue's promo is created
// with no approval whatsoever — it still has to be submitted and approved.
func TestCreate_VenuePromoStillGoesThroughModeration(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), feed)

	if _, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(feed.approvals) != 0 {
		t.Fatalf("approvals = %+v, want a venue promo to reach the moderation queue instead", feed.approvals)
	}

	// Not even when the superadmin creates it on the venue's behalf: the item
	// belongs to a venue, so it travels the venue's path.
	if _, err := f.Create(context.Background(), superadmin(), validCreate(rid)); err != nil {
		t.Fatalf("create as superadmin: %v", err)
	}
	if len(feed.approvals) != 0 {
		t.Fatalf("approvals = %+v, want venue content moderated regardless of who typed it", feed.approvals)
	}
}

// A failed approval is reported, not swallowed: the promo exists at
// not_submitted (the pre-change behaviour) and the caller must learn that its
// card is not live.
func TestCreate_PlatformPromoReportsAFailedApproval(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{approveErr: errors.New("feed down")}
	f := NewFacade(repo, &fakePerms{}, feed)

	if _, err := f.Create(context.Background(), superadmin(), platformCreate()); err == nil {
		t.Fatal("a failed auto-approval must surface, not be swallowed")
	}
}

// The platform editing its OWN card must not send it into a queue it would then
// have to approve for itself — that is the same round trip with nobody on the
// other side.
func TestUpdate_PlatformCardIsNotDemotedByItsOwnEditor(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	p, err := f.Create(context.Background(), superadmin(), platformCreate())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Update(context.Background(), superadmin(), p.ID, UpdateInput{
		Title: "Другой заголовок", StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: domain.PromoPublished,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("demoted = %v, want the platform's own card left on the screen", feed.demoted)
	}
}
