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

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

func validCreate(rid uuid.UUID) CreateInput {
	return CreateInput{
		RestaurantID: rid,
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
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess))

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
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

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
	ev := &domain.Event{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.EventDraft}
	repo.byID[ev.ID] = ev
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleManager))

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.EventPublished}
	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, ev.ID, in)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another restaurant must not edit this event, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a cross-tenant denial")
	}
}

func TestDelete_AdminBypassesPermLookup(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	ev := &domain.Event{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.EventDraft}
	repo.byID[ev.ID] = ev
	// perms would error if consulted — a superadmin must not need it.
	f := NewFacade(repo, &fakePerms{err: errors.New("must not be called")})

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
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner))

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
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleOwner))

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
	draft := &domain.Event{ID: uuid.New(), RestaurantID: rid, Title: "d", Status: domain.EventDraft}
	pub := &domain.Event{ID: uuid.New(), RestaurantID: rid, Title: "p", Status: domain.EventPublished}
	repo.byID[draft.ID] = draft
	repo.byID[pub.ID] = pub
	f := NewFacade(repo, &fakePerms{})

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
