package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// platformCreate is the same payload as validCreate, minus the venue. That
// single nil is what makes the event the platform's own.
func platformCreate() CreateInput {
	return CreateInput{
		Title:    "Городская афиша",
		StartsAt: time.Now().Add(24 * time.Hour),
		EndsAt:   time.Now().Add(27 * time.Hour),
	}
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

// The platform's own event can be created with no venue at all. Before
// migration 0085 this was not expressible: CreateInput carried a bare uuid and
// the column was NOT NULL.
func TestCreate_PlatformEventHasNoVenue(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{err: errors.New("must not be asked about a venue that does not exist")}, &fakeFeed{})

	e, err := f.Create(context.Background(), superadmin(), platformCreate())
	if err != nil {
		t.Fatalf("superadmin must be able to create a platform event: %v", err)
	}
	if e.RestaurantID != nil {
		t.Fatalf("restaurant_id = %v, want nil", e.RestaurantID)
	}
	if !e.IsPlatform() {
		t.Fatal("IsPlatform must report true for a venue-less event")
	}
}

// Authorization for venue-less content is a GLOBAL policy, not a per-restaurant
// permission — there is no restaurant to hold one at. Today that policy is
// superadmin-only, and a venue's own owner (the most privileged staff role)
// must not be able to publish in the platform's name.
func TestCreate_PlatformEventRefusesAVenueRole(t *testing.T) {
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

// The same venue owner keeps every right they had over their OWN venue's
// events. This is the half that must not regress.
func TestCreate_VenueBoundEventIsUnaffected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	e, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if err != nil {
		t.Fatalf("a venue owner must still create their own venue's event: %v", err)
	}
	if e.RestaurantID == nil || *e.RestaurantID != rid {
		t.Fatalf("restaurant_id = %v, want %s", e.RestaurantID, rid)
	}
}

// Editing follows the STORED owner, never the caller's claim: a venue role
// cannot edit a platform event even though it holds restaurant.manage
// somewhere.
func TestUpdate_PlatformEventRefusesAVenueRole(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	ev := &domain.Event{ID: uuid.New(), Title: "Городская афиша", Status: domain.EventDraft}
	repo.byID[ev.ID] = ev
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, ev.ID, UpdateInput{
		Title: "Подменённая афиша", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
		Status: domain.EventPublished,
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if repo.updated != nil {
		t.Fatal("a refused update must write nothing")
	}
}

// A platform event cannot sell tickets: a ticket is a payment and every payment
// in this schema settles to a venue. Refused as a 422 here so the DB CHECK
// never has to answer with a 500.
func TestCreate_PlatformEventCannotBeTicketed(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	price := int64(500000)
	in := platformCreate()
	in.Ticketed = true
	in.TicketPriceMinor = &price

	_, err := f.Create(context.Background(), superadmin(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if repo.created != nil {
		t.Fatal("a refused create must write nothing")
	}
}

// A venue-bound ticketed event is untouched by that rule.
func TestCreate_VenueBoundEventMayStillBeTicketed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	price := int64(500000)
	in := validCreate(rid)
	in.Ticketed = true
	in.TicketPriceMinor = &price

	if _, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in); err != nil {
		t.Fatalf("a venue must still be able to sell tickets: %v", err)
	}
}

// The action button travels through the usecase and is validated there: a
// javascript: link never reaches the store, whatever the client sent.
func TestCreate_ActionURLIsValidated(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	bad := "javascript:alert(document.cookie)"
	in := platformCreate()
	in.Action = &domain.EventAction{Label: "Купить билет", URL: &bad}

	_, err := f.Create(context.Background(), superadmin(), in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if repo.created != nil {
		t.Fatal("an event with a refused link must not be stored at all")
	}

	good := "https://tickets.kz/e/42"
	in.Action = &domain.EventAction{Label: "  Купить билет  ", URL: &good}
	e, err := f.Create(context.Background(), superadmin(), in)
	if err != nil {
		t.Fatalf("a legitimate external link must be accepted: %v", err)
	}
	if e.Action == nil || e.Action.Label != "Купить билет" || e.Action.URL == nil || *e.Action.URL != good {
		t.Fatalf("action = %+v, want the trimmed label and the link", e.Action)
	}
	if e.Action.Target() != domain.EventActionTargetExternal {
		t.Fatalf("target = %q, want external", e.Action.Target())
	}
}

// Repointing an approved card's button at another site is a content edit: the
// platform approved a destination, and changing it must send the card back to
// the moderation queue. Asserted on a VENUE event — the demotion rule guards
// content the platform reviewed for somebody else.
func TestUpdate_RepointingTheButtonDemotesTheCard(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	first := "https://tickets.kz/e/42"
	in := validCreate(uuid.New())
	in.Action = &domain.EventAction{Label: "Купить билет", URL: &first}
	e, err := f.Create(context.Background(), superadmin(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	second := "https://not-tickets.example/e/42"
	_, err = f.Update(context.Background(), superadmin(), e.ID, UpdateInput{
		Title: e.Title, StartsAt: e.StartsAt, EndsAt: e.EndsAt, Status: domain.EventPublished,
		Action: &domain.EventAction{Label: "Купить билет", URL: &second},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 1 || feed.demoted[0] != e.ID {
		t.Fatalf("demoted = %v, want the edited event once", feed.demoted)
	}
}

// Re-saving the SAME button is not an edit and must not cost a re-review.
func TestUpdate_ResavingTheSameButtonIsNotAnEdit(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	link := "https://tickets.kz/e/42"
	in := platformCreate()
	in.Action = &domain.EventAction{Label: "Купить билет", URL: &link}
	e, err := f.Create(context.Background(), superadmin(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	same := link
	if _, err := f.Update(context.Background(), superadmin(), e.ID, UpdateInput{
		Title: e.Title, StartsAt: e.StartsAt, EndsAt: e.EndsAt, Status: e.Status,
		Action: &domain.EventAction{Label: "Купить билет", URL: &same},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("demoted = %v, want nothing: the card did not change", feed.demoted)
	}
}

// The platform cabinet's listing is gated by the same policy as the writes, and
// it answers with the platform's own events only.
func TestListPlatformAdmin(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	if _, err := f.Create(context.Background(), superadmin(), platformCreate()); err != nil {
		t.Fatalf("seed platform event: %v", err)
	}
	if _, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid)); err != nil {
		t.Fatalf("seed venue event: %v", err)
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

// The event's own page answers for a platform event — the read a venue-scoped
// GetPublic cannot express at all — and carries no venue block.
func TestGetPublicDetail_PlatformEvent(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	in := platformCreate()
	in.Status = domain.EventPublished
	e, err := f.Create(context.Background(), superadmin(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	it, err := f.GetPublicDetail(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("GetPublicDetail: %v", err)
	}
	if it.Restaurant != nil {
		t.Fatalf("restaurant = %+v, want nil for a platform event", it.Restaurant)
	}
	if it.ID != e.ID {
		t.Fatalf("id = %s, want %s", it.ID, e.ID)
	}
}

// --- creation-time approval of the platform's own content ---

// The platform's own афиша reaches the home screen without a moderation round
// trip, with the deciding superadmin recorded. Same rule as usecase/promos.
func TestCreate_PlatformEventIsApprovedForTheHomeFeedAtCreation(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	actor := superadmin()
	e, err := f.Create(context.Background(), actor, platformCreate())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(feed.approvals) != 1 {
		t.Fatalf("approvals = %+v, want the platform event approved exactly once", feed.approvals)
	}
	got := feed.approvals[0]
	if got.kind != domain.FeedItemEvent || got.itemID != e.ID || got.reviewer != actor.UserID {
		t.Fatalf("approved %+v, want event %s by %s", got, e.ID, actor.UserID)
	}
}

// Characterisation: a venue's event still needs the platform's decision.
func TestCreate_VenueEventStillGoesThroughModeration(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	if _, err := f.Create(context.Background(), superadmin(), validCreate(rid)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(feed.approvals) != 0 {
		t.Fatalf("approvals = %+v, want a venue event moderated as before", feed.approvals)
	}
}

// The platform editing its own афиша keeps it on the screen.
func TestUpdate_PlatformEventIsNotDemotedByItsOwnEditor(t *testing.T) {
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, &fakePerms{}, feed)

	e, err := f.Create(context.Background(), superadmin(), platformCreate())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Update(context.Background(), superadmin(), e.ID, UpdateInput{
		Title: "Другой заголовок", StartsAt: e.StartsAt, EndsAt: e.EndsAt, Status: domain.EventPublished,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("demoted = %v, want the platform's own card left on the screen", feed.demoted)
	}
}
