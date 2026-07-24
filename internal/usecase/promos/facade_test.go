package promos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakePromoRepo struct {
	byID    map[uuid.UUID]*domain.Promo
	created *domain.Promo
	updated *domain.Promo
	active  []domain.Promo
}

func newFakeRepo() *fakePromoRepo { return &fakePromoRepo{byID: map[uuid.UUID]*domain.Promo{}} }

func (f *fakePromoRepo) Create(_ context.Context, p *domain.Promo) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	f.created = p
	f.byID[p.ID] = p
	return nil
}

func (f *fakePromoRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Promo, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakePromoRepo) Update(_ context.Context, p *domain.Promo) error {
	if _, ok := f.byID[p.ID]; !ok {
		return domain.ErrNotFound
	}
	f.updated = p
	f.byID[p.ID] = p
	return nil
}

func (f *fakePromoRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.byID[id]; !ok {
		return domain.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

func (f *fakePromoRepo) ListByRestaurant(_ context.Context, _ uuid.UUID, _ []domain.PromoStatus, _, _ int) ([]domain.Promo, int, error) {
	return nil, 0, nil
}

func (f *fakePromoRepo) ListActive(_ context.Context, _ uuid.UUID, _ time.Time, _, _ int) ([]domain.Promo, int, error) {
	return f.active, len(f.active), nil
}

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
		Title:        "Happy Hour",
		StartsAt:     time.Now().Add(-time.Hour),
		EndsAt:       time.Now().Add(time.Hour),
	}
}

func TestCreate_HostessForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess))

	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not create a promo, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no promo must be written when a hostess is denied")
	}
}

func TestCreate_ManagerAllowed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	p, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if err != nil {
		t.Fatalf("a manager must be able to create a promo: %v", err)
	}
	if p.Status != domain.PromoDraft {
		t.Fatalf("a new promo must default to draft, got %s", p.Status)
	}
}

func TestUpdate_CrossTenantForbidden(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	pr := &domain.Promo{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.PromoDraft}
	repo.byID[pr.ID] = pr
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleOwner))

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.PromoPublished}
	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, pr.ID, in)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an owner of another restaurant must not edit this promo, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a cross-tenant denial")
	}
}

func TestCreate_InvalidWindowRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	in := validCreate(rid)
	in.EndsAt = in.StartsAt // empty window
	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty window must be ErrValidation, got %v", err)
	}
}
