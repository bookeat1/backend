package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ---

type fakeDraftRepo struct {
	byID       map[uuid.UUID]*domain.ContentDraft
	created    *domain.ContentDraft
	approved   []uuid.UUID
	rejected   []uuid.UUID
	approveErr error
}

func newDraftRepo() *fakeDraftRepo { return &fakeDraftRepo{byID: map[uuid.UUID]*domain.ContentDraft{}} }

func (f *fakeDraftRepo) Create(_ context.Context, d *domain.ContentDraft) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.Status = domain.DraftPendingReview
	f.created = d
	f.byID[d.ID] = d
	return nil
}

func (f *fakeDraftRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.ContentDraft, error) {
	d, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (f *fakeDraftRepo) ListPendingByRestaurant(_ context.Context, rid uuid.UUID, _, _ int) ([]domain.ContentDraft, int, error) {
	var out []domain.ContentDraft
	for _, d := range f.byID {
		if d.RestaurantID == rid && d.Status == domain.DraftPendingReview {
			out = append(out, *d)
		}
	}
	return out, len(out), nil
}

func (f *fakeDraftRepo) MarkApproved(_ context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time, eventID, promoID *uuid.UUID) error {
	if f.approveErr != nil {
		return f.approveErr
	}
	d, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if d.Status != domain.DraftPendingReview {
		return domain.ErrInvalidStatus
	}
	d.Status = domain.DraftApproved
	d.ReviewedBy = &reviewedBy
	d.ReviewedAt = &at
	d.CreatedEventID = eventID
	d.CreatedPromoID = promoID
	f.approved = append(f.approved, id)
	return nil
}

func (f *fakeDraftRepo) MarkRejected(_ context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time) error {
	d, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if d.Status != domain.DraftPendingReview {
		return domain.ErrInvalidStatus
	}
	d.Status = domain.DraftRejected
	d.ReviewedBy = &reviewedBy
	d.ReviewedAt = &at
	f.rejected = append(f.rejected, id)
	return nil
}

type fakeEventRepo struct{ created *domain.Event }

func (f *fakeEventRepo) Create(_ context.Context, e *domain.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	f.created = e
	return nil
}
func (f *fakeEventRepo) GetByID(context.Context, uuid.UUID) (*domain.Event, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeEventRepo) Update(context.Context, *domain.Event) error { return nil }
func (f *fakeEventRepo) Delete(context.Context, uuid.UUID) error     { return nil }
func (f *fakeEventRepo) ListByRestaurant(context.Context, uuid.UUID, []domain.EventStatus, int, int) ([]domain.Event, int, error) {
	return nil, 0, nil
}
func (f *fakeEventRepo) ListPublishedUpcoming(context.Context, uuid.UUID, time.Time, int, int) ([]domain.Event, int, error) {
	return nil, 0, nil
}

type fakePromoRepo struct{ created *domain.Promo }

func (f *fakePromoRepo) Create(_ context.Context, p *domain.Promo) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	f.created = p
	return nil
}
func (f *fakePromoRepo) GetByID(context.Context, uuid.UUID) (*domain.Promo, error) {
	return nil, domain.ErrNotFound
}
func (f *fakePromoRepo) Update(context.Context, *domain.Promo) error { return nil }
func (f *fakePromoRepo) Delete(context.Context, uuid.UUID) error     { return nil }
func (f *fakePromoRepo) ListByRestaurant(context.Context, uuid.UUID, []domain.PromoStatus, int, int) ([]domain.Promo, int, error) {
	return nil, 0, nil
}
func (f *fakePromoRepo) ListActive(context.Context, uuid.UUID, time.Time, int, int) ([]domain.Promo, int, error) {
	return nil, 0, nil
}

// fakeTx runs fn directly (no real transaction).
type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

func (fakeTx) Detach(ctx context.Context) context.Context { return ctx }

type fakePerms struct {
	roles map[[2]uuid.UUID]domain.StaffRole
}

func (f *fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	role, ok := f.roles[[2]uuid.UUID{userID, restaurantID}]
	if !ok {
		return false, nil
	}
	return role.HasPermission(perm), nil
}

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

func pendingEventDraft(rid uuid.UUID) *domain.ContentDraft {
	start := time.Now().Add(24 * time.Hour)
	end := start.Add(2 * time.Hour)
	return &domain.ContentDraft{
		ID:                uuid.New(),
		RestaurantID:      rid,
		Kind:              domain.DraftKindEvent,
		Source:            domain.ContentSourceInstagram,
		Status:            domain.DraftPendingReview,
		SuggestedTitle:    "Parsed Event",
		SuggestedStartsAt: &start,
		SuggestedEndsAt:   &end,
	}
}

// --- tests ---

func TestApprove_ManagerCreatesPublishedEvent(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	pr := &fakePromoRepo{}
	f := NewFacade(drafts, ev, pr, permsWith(actorID, rid, domain.StaffRoleManager), fakeTx{})

	res, err := f.Approve(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if err != nil {
		t.Fatalf("manager approve: %v", err)
	}
	if ev.created == nil {
		t.Fatal("approve must create the real event")
	}
	if ev.created.Status != domain.EventPublished {
		t.Fatalf("approved event must be published, got %s", ev.created.Status)
	}
	if res.EventID == nil || *res.EventID != ev.created.ID {
		t.Fatal("result must carry the created event id")
	}
	if res.PromoID != nil {
		t.Fatal("an event approval must not create a promo")
	}
	if drafts.byID[d.ID].Status != domain.DraftApproved {
		t.Fatal("draft must be marked approved")
	}
}

func TestApprove_HostessForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	f := NewFacade(drafts, ev, &fakePromoRepo{}, permsWith(actorID, rid, domain.StaffRoleHostess), fakeTx{})

	_, err := f.Approve(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not approve a draft, got %v", err)
	}
	if ev.created != nil {
		t.Fatal("no entity must be created on a denial")
	}
	if drafts.byID[d.ID].Status != domain.DraftPendingReview {
		t.Fatal("draft must stay pending on a denial")
	}
}

func TestApprove_CrossTenantForbidden(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	// Manager, but of a DIFFERENT restaurant.
	f := NewFacade(drafts, ev, &fakePromoRepo{}, permsWith(actorID, other, domain.StaffRoleManager), fakeTx{})

	_, err := f.Approve(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another restaurant must not approve this draft, got %v", err)
	}
	if ev.created != nil {
		t.Fatal("no entity must be created on a cross-tenant denial")
	}
}

func TestReject_CreatesNothing(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	pr := &fakePromoRepo{}
	f := NewFacade(drafts, ev, pr, permsWith(actorID, rid, domain.StaffRoleManager), fakeTx{})

	got, err := f.Reject(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if err != nil {
		t.Fatalf("manager reject: %v", err)
	}
	if got.Status != domain.DraftRejected {
		t.Fatalf("draft must be rejected, got %s", got.Status)
	}
	if ev.created != nil || pr.created != nil {
		t.Fatal("reject must create no entity")
	}
}

func TestApprove_NonPendingRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	d.Status = domain.DraftApproved // already reviewed
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	f := NewFacade(drafts, ev, &fakePromoRepo{}, permsWith(actorID, rid, domain.StaffRoleOwner), fakeTx{})

	_, err := f.Approve(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("approving a non-pending draft must be ErrInvalidStatus, got %v", err)
	}
	if ev.created != nil {
		t.Fatal("no entity must be created for a non-pending draft")
	}
}

func TestListPending_TenantScoped(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	f := NewFacade(drafts, &fakeEventRepo{}, &fakePromoRepo{}, permsWith(actorID, rid, domain.StaffRoleManager), fakeTx{})

	// Manager of rid may list rid's queue.
	if _, _, err := f.ListPending(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rid, 1, 20); err != nil {
		t.Fatalf("manager must list own restaurant's queue: %v", err)
	}
	// ...but not another restaurant's.
	if _, _, err := f.ListPending(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, other, 1, 20); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("manager must not list another restaurant's queue, got %v", err)
	}
}

func TestApprove_MissingWindowRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	drafts := newDraftRepo()
	d := pendingEventDraft(rid)
	d.SuggestedEndsAt = nil // incomplete draft
	drafts.byID[d.ID] = d
	ev := &fakeEventRepo{}
	f := NewFacade(drafts, ev, &fakePromoRepo{}, permsWith(actorID, rid, domain.StaffRoleManager), fakeTx{})

	_, err := f.Approve(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, d.ID)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approving a draft with no window must be ErrValidation, got %v", err)
	}
	if ev.created != nil {
		t.Fatal("no entity must be created for an incomplete draft")
	}
}
