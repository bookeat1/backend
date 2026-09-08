package menu

import (
	"context"
	"sort"

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
	featured    []domain.FeaturedMenuItem
	featuredArg domain.FeaturedMenuFilter
	featuredID  uuid.UUID
	featuredSet bool
	featuredErr error

	// list backs ListByRestaurant. Order matters: the repository returns items
	// already sorted by display_order NULLS LAST, name, and the highlights rule
	// relies on that, so the fake hands back exactly what it was given.
	list     []domain.MenuItem
	listArg  domain.MenuItemFilter
	listErr  error
	cleared  int
	setSlots []setSlotCall
	// slotErrOnce makes the NEXT SetTopPickPosition fail with the given error
	// and then clears itself — that is how a lost slot race is reproduced.
	slotErrOnce error
}

// setSlotCall records one SetTopPickPosition call so a test can assert the
// order the rail was written in, not just its final state.
type setSlotCall struct {
	itemID   uuid.UUID
	position *int
}

func newFakeItems() *fakeItems {
	return &fakeItems{store: map[uuid.UUID]*domain.MenuItem{}, tagsFor: map[uuid.UUID][]domain.MenuItemTag{}}
}

func (f *fakeItems) ListByRestaurant(_ context.Context, flt domain.MenuItemFilter) ([]domain.MenuItem, error) {
	f.listArg = flt
	return f.list, f.listErr
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
func (f *fakeItems) SetAvailableBulk(_ context.Context, restaurantID uuid.UUID, ids []uuid.UUID, a bool) (int, error) {
	n := 0
	for _, id := range ids {
		if m, ok := f.store[id]; ok && m.RestaurantID == restaurantID {
			m.IsAvailable = a
			n++
		}
	}
	f.avail = a
	return n, nil
}
func (f *fakeItems) ListFeatured(_ context.Context, flt domain.FeaturedMenuFilter) ([]domain.FeaturedMenuItem, error) {
	f.featuredArg = flt
	return f.featured, f.featuredErr
}
func (f *fakeItems) SetFeatured(_ context.Context, restaurantID, id uuid.UUID, featured bool) error {
	m, ok := f.store[id]
	if !ok || m.RestaurantID != restaurantID {
		return domain.ErrNotFound
	}
	m.IsFeatured = featured
	f.featuredID, f.featuredSet = id, featured
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

// ListTopPicks mirrors the repository: marked dishes of this venue only,
// ordered by slot, availability NOT filtered.
func (f *fakeItems) ListTopPicks(_ context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error) {
	out := []domain.MenuItem{}
	for _, m := range f.store {
		if m.RestaurantID == restaurantID && m.TopPickPosition != nil {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].TopPickPosition < *out[j].TopPickPosition })
	return out, nil
}

// SetTopPickPosition mirrors the repository's tenant guard AND its partial
// UNIQUE index: a slot already held by another dish of the same venue comes
// back as ErrAlreadyExists, not as a silent overwrite.
func (f *fakeItems) SetTopPickPosition(_ context.Context, restaurantID, id uuid.UUID, position *int) error {
	if f.slotErrOnce != nil {
		err := f.slotErrOnce
		f.slotErrOnce = nil
		return err
	}
	m, ok := f.store[id]
	if !ok || m.RestaurantID != restaurantID {
		return domain.ErrNotFound
	}
	if position != nil {
		for other, om := range f.store {
			if other != id && om.RestaurantID == restaurantID &&
				om.TopPickPosition != nil && *om.TopPickPosition == *position {
				return domain.ErrAlreadyExists
			}
		}
	}
	m.TopPickPosition = position
	f.setSlots = append(f.setSlots, setSlotCall{itemID: id, position: position})
	return nil
}

func (f *fakeItems) ClearTopPicks(_ context.Context, restaurantID uuid.UUID) (int, error) {
	n := 0
	for _, m := range f.store {
		if m.RestaurantID == restaurantID && m.TopPickPosition != nil {
			m.TopPickPosition = nil
			n++
		}
	}
	f.cleared++
	return n, nil
}

func intp(i int) *int { return &i }
