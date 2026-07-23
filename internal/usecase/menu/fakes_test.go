package menu

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeItems struct {
	store       map[uuid.UUID]*domain.MenuItem
	created     *domain.MenuItem
	updated     *domain.MenuItem
	deleted     uuid.UUID
	availID     uuid.UUID
	avail       bool
	tagsFor     map[uuid.UUID][]domain.MenuItemTag
	replaceCall int
}

func newFakeItems() *fakeItems {
	return &fakeItems{store: map[uuid.UUID]*domain.MenuItem{}, tagsFor: map[uuid.UUID][]domain.MenuItemTag{}}
}

func (f *fakeItems) ListByRestaurant(_ context.Context, _ domain.MenuItemFilter) ([]domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeItems) GetByID(_ context.Context, id uuid.UUID) (*domain.MenuItem, error) {
	if m, ok := f.store[id]; ok {
		return m, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeItems) Create(_ context.Context, m *domain.MenuItem) error {
	f.created = m
	f.store[m.ID] = m
	return nil
}
func (f *fakeItems) Update(_ context.Context, m *domain.MenuItem) error {
	f.updated = m
	f.store[m.ID] = m
	return nil
}
func (f *fakeItems) Delete(_ context.Context, id uuid.UUID) error { f.deleted = id; return nil }
func (f *fakeItems) SetAvailable(_ context.Context, id uuid.UUID, a bool) error {
	f.availID, f.avail = id, a
	return nil
}
func (f *fakeItems) ReplaceTags(_ context.Context, itemID uuid.UUID, tags []domain.MenuItemTag) error {
	f.replaceCall++
	f.tagsFor[itemID] = tags
	return nil
}

type fakeCategories struct {
	created, updated *domain.MenuCategory
	deleted          uuid.UUID
	list             []domain.MenuCategory
}

func (f *fakeCategories) List(context.Context) ([]domain.MenuCategory, error) { return f.list, nil }
func (f *fakeCategories) Create(_ context.Context, c *domain.MenuCategory) error {
	f.created = c
	return nil
}
func (f *fakeCategories) Update(_ context.Context, c *domain.MenuCategory) error {
	f.updated = c
	return nil
}
func (f *fakeCategories) Delete(_ context.Context, id uuid.UUID) error { f.deleted = id; return nil }

type inlineTx struct{ called bool }

func (t *inlineTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.called = true
	return fn(ctx)
}

func strp(s string) *string { return &s }

func (t *inlineTx) Detach(ctx context.Context) context.Context { return ctx }
