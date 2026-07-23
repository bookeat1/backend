package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeRestaurantRepo struct {
	created  *domain.Restaurant
	updated  *domain.Restaurant
	getErr   error
	agg      *domain.RestaurantAggregate
	list     []domain.RestaurantListItem
	total    int
	activeID uuid.UUID
	active   bool
	policyID uuid.UUID
	policy   domain.BookingPolicyOverride
}

func (f *fakeRestaurantRepo) Create(_ context.Context, r *domain.Restaurant) error {
	f.created = r
	return nil
}
func (f *fakeRestaurantRepo) Update(_ context.Context, r *domain.Restaurant) error {
	f.updated = r
	return nil
}
func (f *fakeRestaurantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.agg == nil {
		return &domain.RestaurantAggregate{}, nil
	}
	return f.agg, nil
}
func (f *fakeRestaurantRepo) UpdateBookingPolicy(_ context.Context, id uuid.UUID, o domain.BookingPolicyOverride) error {
	f.policyID, f.policy = id, o
	return nil
}
func (f *fakeRestaurantRepo) ListActive(_ context.Context, _ domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	return f.list, f.total, nil
}
func (f *fakeRestaurantRepo) Search(_ context.Context, _ domain.RestaurantSearchFilter) ([]domain.RestaurantListItem, int, error) {
	return f.list, f.total, nil
}
func (f *fakeRestaurantRepo) SetActive(_ context.Context, id uuid.UUID, a bool) error {
	f.activeID, f.active = id, a
	return nil
}

// fakeRelated records both the total number of Replace* calls (replaced, kept
// for backward-compat with existing assertions) and which specific
// collections were touched, so tests can assert that Update only replaces
// the collections explicitly provided in the request.
type fakeRelated struct {
	replaced int

	imagesReplaced      bool
	featuresReplaced    bool
	tagsReplaced        bool
	socialLinksReplaced bool
}

func (f *fakeRelated) ListImages(context.Context, uuid.UUID) ([]domain.Image, error) { return nil, nil }
func (f *fakeRelated) ListFeatures(context.Context, uuid.UUID) ([]domain.Feature, error) {
	return nil, nil
}
func (f *fakeRelated) ListTags(context.Context, uuid.UUID) ([]domain.Tag, error) { return nil, nil }
func (f *fakeRelated) ListSocialLinks(context.Context, uuid.UUID) ([]domain.SocialLink, error) {
	return nil, nil
}
func (f *fakeRelated) ListWorkingHours(context.Context, uuid.UUID) ([]domain.WorkingHours, error) {
	return nil, nil
}
func (f *fakeRelated) ListTimeSlots(context.Context, uuid.UUID) ([]domain.TimeSlot, error) {
	return nil, nil
}
func (f *fakeRelated) ListTables(context.Context, uuid.UUID) ([]domain.RestaurantTable, error) {
	return nil, nil
}
func (f *fakeRelated) GetFloorPlan(context.Context, uuid.UUID) (*domain.FloorPlan, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeRelated) ReplaceImages(context.Context, uuid.UUID, []domain.Image) error {
	f.replaced++
	f.imagesReplaced = true
	return nil
}
func (f *fakeRelated) ReplaceFeatures(context.Context, uuid.UUID, []domain.Feature) error {
	f.replaced++
	f.featuresReplaced = true
	return nil
}
func (f *fakeRelated) ReplaceTags(context.Context, uuid.UUID, []domain.Tag) error {
	f.replaced++
	f.tagsReplaced = true
	return nil
}
func (f *fakeRelated) ReplaceSocialLinks(context.Context, uuid.UUID, []domain.SocialLink) error {
	f.replaced++
	f.socialLinksReplaced = true
	return nil
}
func (f *fakeRelated) ReplaceWorkingHours(context.Context, uuid.UUID, []domain.WorkingHours) error {
	return nil
}
func (f *fakeRelated) ReplaceTimeSlots(context.Context, uuid.UUID, []domain.TimeSlot) error {
	return nil
}
func (f *fakeRelated) ReplaceTables(context.Context, uuid.UUID, []domain.RestaurantTable) error {
	return nil
}
func (f *fakeRelated) UpsertFloorPlan(context.Context, *domain.FloorPlan) error { return nil }

type fakeCategories struct{ items []domain.RestaurantCategory }

func (f *fakeCategories) List(context.Context) ([]domain.RestaurantCategory, error) {
	return f.items, nil
}
func (f *fakeCategories) Create(context.Context, *domain.RestaurantCategory) error { return nil }

type fakePartners struct{ created *domain.PartnershipRequest }

func (f *fakePartners) Create(_ context.Context, p *domain.PartnershipRequest) error {
	f.created = p
	return nil
}

// fakeManagers is a hand-written domain.RestaurantManagerRepository backed by
// a single mutable slice, close enough to the real table to exercise
// ManagerUseCase's authorization logic (List/Assign/SetRole/Remove all
// resolve a row by id or filter by user/restaurant, same as Postgres would).
type fakeManagers struct {
	rows       []domain.RestaurantManager
	created    *domain.RestaurantManager
	getErr     error
	createErr  error
	updRoleErr error
	delErr     error
}

func (f *fakeManagers) ListByRestaurant(_ context.Context, rid uuid.UUID) ([]domain.RestaurantManager, error) {
	var out []domain.RestaurantManager
	for _, m := range f.rows {
		if m.RestaurantID == rid {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeManagers) ListByUser(_ context.Context, uid uuid.UUID) ([]domain.RestaurantManager, error) {
	var out []domain.RestaurantManager
	for _, m := range f.rows {
		if m.UserID == uid {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeManagers) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantManager, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			m := f.rows[i]
			return &m, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeManagers) Create(_ context.Context, m *domain.RestaurantManager) error {
	if f.createErr != nil {
		return f.createErr
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	f.created = m
	f.rows = append(f.rows, *m)
	return nil
}

func (f *fakeManagers) UpdateRole(_ context.Context, id uuid.UUID, role domain.StaffRole) error {
	if f.updRoleErr != nil {
		return f.updRoleErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Role = role
			return nil
		}
	}
	return domain.ErrNotFound
}

func (f *fakeManagers) Delete(_ context.Context, id uuid.UUID) error {
	if f.delErr != nil {
		return f.delErr
	}
	for i, m := range f.rows {
		if m.ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

// fakeUsers is a hand-written userRepo.
type fakeUsers struct {
	err       error
	updateErr error
	user      *domain.User // optional override for GetByID's result
	updated   *domain.User
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.user != nil {
		return f.user, nil
	}
	return &domain.User{ID: id, Role: domain.RoleUser}, nil
}

func (f *fakeUsers) Update(_ context.Context, u *domain.User) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = u
	return nil
}

// inlineTx runs fn directly (no real transaction) for unit tests.
type inlineTx struct{ called bool }

func (t *inlineTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.called = true
	return fn(ctx)
}

func (t *inlineTx) Detach(ctx context.Context) context.Context { return ctx }
