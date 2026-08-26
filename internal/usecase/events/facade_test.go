package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ---

type fakeEventRepo struct {
	byID      map[uuid.UUID]*domain.Event
	created   *domain.Event
	updated   *domain.Event
	deleted   []uuid.UUID
	published []domain.Event
	createErr error

	// cross-venue public listing: what the repository was asked for, and what
	// it answers with.
	publicItems  []domain.EventListItem
	publicFilter domain.PublicEventFilter
	publicNow    time.Time
	publicCalls  int
	publicErr    error
	// venue is the host-venue block GetPublicByID hands back; nil = a platform
	// event, which is the default here.
	venue *domain.EventRestaurant
}

func newFakeRepo() *fakeEventRepo { return &fakeEventRepo{byID: map[uuid.UUID]*domain.Event{}} }

func (f *fakeEventRepo) Create(_ context.Context, e *domain.Event) error {
	if f.createErr != nil {
		return f.createErr
	}
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	f.created = e
	f.byID[e.ID] = e
	return nil
}

func (f *fakeEventRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Event, error) {
	e, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (f *fakeEventRepo) Update(_ context.Context, e *domain.Event) error {
	if _, ok := f.byID[e.ID]; !ok {
		return domain.ErrNotFound
	}
	f.updated = e
	f.byID[e.ID] = e
	return nil
}

func (f *fakeEventRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.byID[id]; !ok {
		return domain.ErrNotFound
	}
	f.deleted = append(f.deleted, id)
	delete(f.byID, id)
	return nil
}

func (f *fakeEventRepo) ListByRestaurant(_ context.Context, _ uuid.UUID, _ []domain.EventStatus, _, _ int) ([]domain.Event, int, error) {
	return nil, 0, nil
}

func (f *fakeEventRepo) ListPublishedUpcoming(_ context.Context, _ uuid.UUID, _ time.Time, _, _ int) ([]domain.Event, int, error) {
	return f.published, len(f.published), nil
}

// ListPlatform answers from the same store Create writes to, filtered to the
// events that have no host venue — so a "create a platform event, then list the
// platform's events" test exercises the real predicate, not a canned slice.
func (f *fakeEventRepo) ListPlatform(_ context.Context, statuses []domain.EventStatus, _, _ int) ([]domain.Event, int, error) {
	var out []domain.Event
	for _, e := range f.byID {
		if e.RestaurantID != nil {
			continue
		}
		if len(statuses) > 0 {
			match := false
			for _, s := range statuses {
				if e.Status == s {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, *e)
	}
	return out, len(out), nil
}

// GetPublicByID applies the same visibility rule the SQL does: published, not
// yet ended. The venue block comes from f.venue, nil by default — which is
// exactly the platform-event shape.
func (f *fakeEventRepo) GetPublicByID(_ context.Context, id uuid.UUID, now time.Time) (*domain.EventListItem, error) {
	e, ok := f.byID[id]
	if !ok || e.Status != domain.EventPublished || !e.EndsAt.After(now) {
		return nil, domain.ErrNotFound
	}
	cp := *e
	return &domain.EventListItem{Event: cp, Restaurant: f.venue}, nil
}

func (f *fakeEventRepo) ListPublicUpcoming(_ context.Context, flt domain.PublicEventFilter, now time.Time) ([]domain.EventListItem, int, error) {
	f.publicCalls++
	f.publicFilter = flt
	f.publicNow = now
	if f.publicErr != nil {
		return nil, 0, f.publicErr
	}
	return f.publicItems, len(f.publicItems), nil
}

// fakePerms answers HasPermission from a fixed (userID,restaurantID)->role map.
type fakePerms struct {
	roles map[[2]uuid.UUID]domain.StaffRole
	err   error
}

func (f *fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	role, ok := f.roles[[2]uuid.UUID{userID, restaurantID}]
	if !ok {
		return false, nil
	}
	return role.HasPermission(perm), nil
}

// fakeFeed records the demotions the facade asks for when content changes.
type fakeFeed struct{ demoted []uuid.UUID }

func (f *fakeFeed) DemoteAfterContentEdit(_ context.Context, _ domain.FeedItemKind, itemID uuid.UUID) error {
	f.demoted = append(f.demoted, itemID)
	return nil
}

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

func validCreate(rid uuid.UUID) CreateInput {
	return CreateInput{
		RestaurantID: &rid,
		Title:        "Wine Dinner",
		StartsAt:     time.Now().Add(24 * time.Hour),
		EndsAt:       time.Now().Add(27 * time.Hour),
	}
}

// --- authorization ---

func TestCreate_HostessForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess), &fakeFeed{})

	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not create an event, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no event must be written when a hostess is denied")
	}
}

func TestCreate_ManagerAllowed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	e, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if err != nil {
		t.Fatalf("a manager must be able to create an event: %v", err)
	}
	if e.Status != domain.EventDraft {
		t.Fatalf("a new event must default to draft, got %s", e.Status)
	}
	if repo.created == nil {
		t.Fatal("event must be written")
	}
}

func TestUpdate_CrossTenantForbidden(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	// Event belongs to rid; actor is a manager of a DIFFERENT restaurant.
	ev := &domain.Event{ID: uuid.New(), RestaurantID: &rid, Title: "x", Status: domain.EventDraft}
	repo.byID[ev.ID] = ev
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleManager), &fakeFeed{})

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.EventPublished}
	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, ev.ID, in)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another restaurant must not edit this event, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a cross-tenant denial")
	}
}

// A full-replace Update that says nothing about the refund rules must LEAVE
// THEM ALONE. Without this, an older cabinet build editing a title would switch
// a venue's refunds off for every future buyer, silently — the failure a review
// flagged on the money path.
func TestUpdate_AbsentRefundPolicyKeepsTheVenuesRules(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	ev := &domain.Event{
		ID: uuid.New(), RestaurantID: &rid, Title: "x", Status: domain.EventDraft,
		TicketsRefundable: true, TicketRefundCutoffMinutes: 1440,
	}
	repo.byID[ev.ID] = ev
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	in := UpdateInput{
		Title: "новое название", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
		Status: domain.EventDraft, RefundPolicy: nil, // the older client sends no rules
	}
	got, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, ev.ID, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !got.TicketsRefundable || got.TicketRefundCutoffMinutes != 1440 {
		t.Fatalf("refund rules were clobbered by an unrelated edit: refundable=%v cutoff=%d",
			got.TicketsRefundable, got.TicketRefundCutoffMinutes)
	}
}

// The same call WITH rules attached still replaces them — the venue can turn
// refunds off deliberately, it just cannot do it by accident.
func TestUpdate_PresentRefundPolicyReplacesTheRules(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	ev := &domain.Event{
		ID: uuid.New(), RestaurantID: &rid, Title: "x", Status: domain.EventDraft,
		TicketsRefundable: true, TicketRefundCutoffMinutes: 1440,
	}
	repo.byID[ev.ID] = ev
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	in := UpdateInput{
		Title: "x", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.EventDraft,
		RefundPolicy: &domain.TicketRefundPolicy{Refundable: false, CutoffMinutes: 0},
	}
	got, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, ev.ID, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.TicketsRefundable || got.TicketRefundCutoffMinutes != 0 {
		t.Fatalf("an explicit policy must win: refundable=%v cutoff=%d",
			got.TicketsRefundable, got.TicketRefundCutoffMinutes)
	}
}

func TestDelete_AdminBypassesPermLookup(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	ev := &domain.Event{ID: uuid.New(), RestaurantID: &rid, Title: "x", Status: domain.EventDraft}
	repo.byID[ev.ID] = ev
	// perms would error if consulted — a superadmin must not need it.
	f := NewFacade(repo, &fakePerms{err: errors.New("must not be called")}, &fakeFeed{})

	if err := f.Delete(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, ev.ID); err != nil {
		t.Fatalf("superadmin must delete without a perm lookup: %v", err)
	}
	if len(repo.deleted) != 1 {
		t.Fatal("event must be deleted")
	}
}

// --- validation ---

func TestCreate_InvalidWindowRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	in := validCreate(rid)
	in.EndsAt = in.StartsAt.Add(-time.Hour) // ends before starts
	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ends<starts must be ErrValidation, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no event must be written for an invalid window")
	}
}

func TestCreate_EmptyTitleRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner), &fakeFeed{})

	in := validCreate(rid)
	in.Title = "   "
	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank title must be ErrValidation, got %v", err)
	}
}

// --- public read scoping ---

func TestGetPublic_HidesDraftAndCrossTenant(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	draft := &domain.Event{ID: uuid.New(), RestaurantID: &rid, Title: "d", Status: domain.EventDraft}
	pub := &domain.Event{ID: uuid.New(), RestaurantID: &rid, Title: "p", Status: domain.EventPublished}
	repo.byID[draft.ID] = draft
	repo.byID[pub.ID] = pub
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	if _, err := f.GetPublic(context.Background(), rid, draft.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a draft must not be publicly readable, got %v", err)
	}
	if _, err := f.GetPublic(context.Background(), uuid.New(), pub.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a published event of another restaurant must be NotFound, got %v", err)
	}
	got, err := f.GetPublic(context.Background(), rid, pub.ID)
	if err != nil {
		t.Fatalf("a published event of this restaurant must be readable: %v", err)
	}
	if got.ID != pub.ID {
		t.Fatal("wrong event returned")
	}
}

// --- cross-venue public listing (Explore screen) ---

// The rule "what a guest may see" (published, not yet ended, active venue) is
// enforced in SQL, so what this layer must be held to is narrower and just as
// important: it passes the caller's filters through UNCHANGED, adds no
// visibility knob of its own, and supplies the clock the repository compares
// ends_at against. A regression here would either widen visibility or freeze
// the "finished" cut-off.
func TestListPublicUpcoming_FiltersReachTheRepositoryUnchanged(t *testing.T) {
	rid := uuid.New()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	city := domain.CityAlmaty

	cases := map[string]domain.PublicEventFilter{
		"no filters":      {Page: 1, PerPage: 20},
		"city":            {City: &city, Page: 1, PerPage: 20},
		"restaurant":      {RestaurantID: &rid, Page: 1, PerPage: 20},
		"date range":      {From: &from, To: &to, Page: 1, PerPage: 20},
		"open-ended from": {From: &from, Page: 1, PerPage: 20},
		"open-ended to":   {To: &to, Page: 1, PerPage: 20},
		"everything at once": {
			City: &city, RestaurantID: &rid, From: &from, To: &to, Page: 3, PerPage: 5,
		},
	}
	for name, flt := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newFakeRepo()
			f := NewFacade(repo, &fakePerms{}, &fakeFeed{})
			fixed := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
			f.(*facade).clock = func() time.Time { return fixed }

			if _, _, err := f.ListPublicUpcoming(context.Background(), flt); err != nil {
				t.Fatalf("ListPublicUpcoming: %v", err)
			}
			if repo.publicCalls != 1 {
				t.Fatalf("repository calls = %d, want exactly 1", repo.publicCalls)
			}
			if !filterEqual(repo.publicFilter, flt) {
				t.Fatalf("filter reached the repository as %+v, want %+v", repo.publicFilter, flt)
			}
			if !repo.publicNow.Equal(fixed) {
				t.Fatalf("now = %v, want the facade clock %v", repo.publicNow, fixed)
			}
		})
	}
}

// An inverted range is a caller mistake, not an empty page: it must be named
// (422) and must never reach the database.
func TestListPublicUpcoming_InvertedRangeRejected(t *testing.T) {
	from := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(-24 * time.Hour)
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	_, _, err := f.ListPublicUpcoming(context.Background(), domain.PublicEventFilter{From: &from, To: &to})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("to before from must be ErrValidation, got %v", err)
	}
	if repo.publicCalls != 0 {
		t.Fatal("an invalid range must not reach the repository")
	}
	// The degenerate from == to is a legitimate one-instant query, not an error.
	same := from
	if _, _, err := f.ListPublicUpcoming(context.Background(), domain.PublicEventFilter{From: &from, To: &same}); err != nil {
		t.Fatalf("from == to must be accepted, got %v", err)
	}
}

// The listing carries the host venue with each item — the Explore card needs it
// and must not have to fetch a restaurant per row.
func TestListPublicUpcoming_ItemsCarryTheirVenue(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	repo.publicItems = []domain.EventListItem{{
		Event:      domain.Event{ID: uuid.New(), RestaurantID: &rid, Title: "Wine Dinner", Status: domain.EventPublished},
		Restaurant: &domain.EventRestaurant{ID: rid, Name: "Bistro", City: domain.CityAlmaty},
	}}
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	items, total, err := f.ListPublicUpcoming(context.Background(), domain.PublicEventFilter{})
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(items))
	}
	if items[0].Restaurant.ID != rid || items[0].Restaurant.Name != "Bistro" || items[0].Restaurant.City != domain.CityAlmaty {
		t.Fatalf("venue not carried through: %+v", items[0].Restaurant)
	}
}

func TestListPublicUpcoming_RepositoryErrorPropagates(t *testing.T) {
	repo := newFakeRepo()
	repo.publicErr = errors.New("boom")
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{})

	if _, _, err := f.ListPublicUpcoming(context.Background(), domain.PublicEventFilter{}); err == nil {
		t.Fatal("a repository failure must not be swallowed into an empty page")
	}
}

func filterEqual(a, b domain.PublicEventFilter) bool {
	cityEq := (a.City == nil) == (b.City == nil) && (a.City == nil || *a.City == *b.City)
	ridEq := (a.RestaurantID == nil) == (b.RestaurantID == nil) && (a.RestaurantID == nil || *a.RestaurantID == *b.RestaurantID)
	fromEq := (a.From == nil) == (b.From == nil) && (a.From == nil || a.From.Equal(*b.From))
	toEq := (a.To == nil) == (b.To == nil) && (a.To == nil || a.To.Equal(*b.To))
	return cityEq && ridEq && fromEq && toEq && a.Page == b.Page && a.PerPage == b.PerPage
}

// Галерея в этих тестах не участвует: фейк принимает запись и отдаёт пустую
// выборку — ровно столько, сколько нужно, чтобы удовлетворить интерфейс.
func (f *fakeEventRepo) ReplaceImages(_ context.Context, _ uuid.UUID, _ []string) error { return nil }

func (f *fakeEventRepo) ImagesByEvent(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]string, error) {
	return map[uuid.UUID][]string{}, nil
}
