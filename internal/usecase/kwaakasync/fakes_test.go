package kwaakasync

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeRestaurants struct {
	linked []domain.KwaakaLinkedRestaurant
	err    error
}

func (f *fakeRestaurants) ListKwaakaLinked(ctx context.Context) ([]domain.KwaakaLinkedRestaurant, error) {
	return f.linked, f.err
}

// fakeFetcher answers FetchMenu per kwaakaRestaurantID, so a multi-restaurant
// test can give each venue a different (or failing) menu.
type fakeFetcher struct {
	menus map[string]domain.KwaakaMenu
	errs  map[string]error
	calls []string
}

func (f *fakeFetcher) FetchMenu(ctx context.Context, kwaakaRestaurantID string) (domain.KwaakaMenu, error) {
	f.calls = append(f.calls, kwaakaRestaurantID)
	if err, ok := f.errs[kwaakaRestaurantID]; ok {
		return domain.KwaakaMenu{}, err
	}
	return f.menus[kwaakaRestaurantID], nil
}

// fakeItems is a minimal in-memory domain.MenuItemRepository. Only the two
// methods the worker calls are exercised meaningfully; every other method
// exists solely to satisfy the interface.
type fakeItems struct {
	upserted    map[string]domain.MenuItem // keyed by restaurantID.String()+"/"+kwaakaProductID
	upsertErrOn string                     // fails UpsertFromKwaaka when it sees this kwaaka product id
	stopCalls   []stopCall
	stopErr     error
}

type stopCall struct {
	restaurantID uuid.UUID
	keep         []string
}

func newFakeItems() *fakeItems { return &fakeItems{upserted: map[string]domain.MenuItem{}} }

func (f *fakeItems) key(restaurantID uuid.UUID, kwaakaProductID string) string {
	return restaurantID.String() + "/" + kwaakaProductID
}

func (f *fakeItems) UpsertFromKwaaka(ctx context.Context, m *domain.MenuItem) error {
	if m.KwaakaProductID != nil && *m.KwaakaProductID == f.upsertErrOn {
		return domain.ErrUnavailable
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	f.upserted[f.key(m.RestaurantID, *m.KwaakaProductID)] = *m
	return nil
}

func (f *fakeItems) MarkUnavailableExceptKwaakaIDs(ctx context.Context, restaurantID uuid.UUID, keep []string) (int, error) {
	f.stopCalls = append(f.stopCalls, stopCall{restaurantID: restaurantID, keep: keep})
	if f.stopErr != nil {
		return 0, f.stopErr
	}
	return len(keep), nil // arbitrary but deterministic for assertions
}

// The rest of domain.MenuItemRepository — unused by the worker, kept as
// no-ops so *fakeItems satisfies the interface.
func (f *fakeItems) ListByRestaurant(ctx context.Context, _ domain.MenuItemFilter) ([]domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeItems) GetByID(ctx context.Context, id uuid.UUID) (*domain.MenuItem, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeItems) Create(ctx context.Context, m *domain.MenuItem) error { return nil }
func (f *fakeItems) Update(ctx context.Context, m *domain.MenuItem) error { return nil }
func (f *fakeItems) Delete(ctx context.Context, id uuid.UUID) error       { return nil }
func (f *fakeItems) SetAvailable(ctx context.Context, id uuid.UUID, available bool) error {
	return nil
}
func (f *fakeItems) ListFeatured(ctx context.Context, _ domain.FeaturedMenuFilter) ([]domain.FeaturedMenuItem, error) {
	return nil, nil
}
func (f *fakeItems) SetFeatured(ctx context.Context, restaurantID, id uuid.UUID, featured bool) error {
	return nil
}
func (f *fakeItems) SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID, available bool) (int, error) {
	return 0, nil
}
func (f *fakeItems) ListTopPicks(ctx context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeItems) SetTopPickPosition(ctx context.Context, restaurantID, id uuid.UUID, position *int) error {
	return nil
}
func (f *fakeItems) ClearTopPicks(ctx context.Context, restaurantID uuid.UUID) (int, error) {
	return 0, nil
}
func (f *fakeItems) ReplaceTags(ctx context.Context, menuItemID uuid.UUID, tags []domain.MenuItemTag) error {
	return nil
}

type inlineTx struct{}

func (inlineTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (inlineTx) Detach(ctx context.Context) context.Context                         { return ctx }
