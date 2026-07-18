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
func (f *fakeRestaurantRepo) ListActive(_ context.Context, _ domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
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

type fakeManagers struct {
	byUser  []domain.RestaurantManager
	created *domain.RestaurantManager
	delErr  error
}

func (f *fakeManagers) ListByRestaurant(context.Context, uuid.UUID) ([]domain.RestaurantManager, error) {
	return nil, nil
}
func (f *fakeManagers) ListByUser(context.Context, uuid.UUID) ([]domain.RestaurantManager, error) {
	return f.byUser, nil
}
func (f *fakeManagers) Create(_ context.Context, m *domain.RestaurantManager) error {
	f.created = m
	return nil
}
func (f *fakeManagers) Delete(context.Context, uuid.UUID) error { return f.delErr }

type fakeUsers struct{ err error }

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.User{ID: id}, nil
}

// inlineTx runs fn directly (no real transaction) for unit tests.
type inlineTx struct{ called bool }

func (t *inlineTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.called = true
	return fn(ctx)
}
