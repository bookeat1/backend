package menu

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes menu reads and per-restaurant mutations. Mutating methods take
// restaurantID (from the route) and enforce that the item belongs to it (IDOR).
type Facade interface {
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, lang *string) ([]domain.MenuItem, error)
	Get(ctx context.Context, itemID uuid.UUID) (*domain.MenuItem, error)
	Categories(ctx context.Context) ([]domain.MenuCategory, error)

	Create(ctx context.Context, restaurantID uuid.UUID, in ItemInput) (*domain.MenuItem, error)
	Update(ctx context.Context, restaurantID, itemID uuid.UUID, in ItemInput) (*domain.MenuItem, error)
	Delete(ctx context.Context, restaurantID, itemID uuid.UUID) error
	SetAvailable(ctx context.Context, restaurantID, itemID uuid.UUID, available bool) error

	CreateCategory(ctx context.Context, in CategoryInput) (*domain.MenuCategory, error)
	UpdateCategory(ctx context.Context, id uuid.UUID, in CategoryInput) (*domain.MenuCategory, error)
	DeleteCategory(ctx context.Context, id uuid.UUID) error
}

type facade struct {
	items      domain.MenuItemRepository
	categories domain.MenuCategoryRepository
	tx         domain.TxManager
}

// NewFacade constructs the menu Facade.
func NewFacade(items domain.MenuItemRepository, categories domain.MenuCategoryRepository, tx domain.TxManager) Facade {
	return &facade{items: items, categories: categories, tx: tx}
}

// ItemInput carries mutable menu-item fields. On Update a nil pointer leaves the
// existing value unchanged; Tags nil leaves existing tags untouched, non-nil
// replaces them (opt-in, so a PATCH that omits "tags" preserves them).
type ItemInput struct {
	Name            *string
	NameI18n        domain.I18n
	Description     *string
	DescriptionI18n domain.I18n
	Price           *string
	ImageURL        *string
	IsAvailable     *bool
	Category        *string
	CategoryI18n    domain.I18n
	Subcategory     *string
	SubcategoryI18n domain.I18n
	PortionSize     *string
	PortionSizeI18n domain.I18n
	Language        *string
	DisplayOrder    *int
	Tags            *[]string
}

// CategoryInput carries mutable menu-category fields.
type CategoryInput struct {
	Name         string
	NameI18n     domain.I18n
	ParentID     *uuid.UUID
	DisplayOrder int
}

func (f *facade) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, lang *string) ([]domain.MenuItem, error) {
	return f.items.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: restaurantID, Language: lang})
}

func (f *facade) Get(ctx context.Context, itemID uuid.UUID) (*domain.MenuItem, error) {
	return f.items.GetByID(ctx, itemID)
}

func (f *facade) Categories(ctx context.Context) ([]domain.MenuCategory, error) {
	return f.categories.List(ctx)
}

func (f *facade) Create(ctx context.Context, restaurantID uuid.UUID, in ItemInput) (*domain.MenuItem, error) {
	if in.Name == nil || in.Price == nil {
		return nil, domain.ErrValidation
	}
	if *in.Name == "" || !domain.ValidPrice(*in.Price) {
		return nil, domain.ErrValidation
	}
	m := &domain.MenuItem{ID: uuid.New(), RestaurantID: restaurantID, IsAvailable: true}
	applyItem(m, in)
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.items.Create(ctx, m); err != nil {
			return err
		}
		return f.items.ReplaceTags(ctx, m.ID, tagsOf(m.ID, in.Tags))
	})
	if err != nil {
		return nil, err
	}
	return f.items.GetByID(ctx, m.ID)
}

func (f *facade) Update(ctx context.Context, restaurantID, itemID uuid.UUID, in ItemInput) (*domain.MenuItem, error) {
	if in.Price != nil && !domain.ValidPrice(*in.Price) {
		return nil, domain.ErrValidation
	}
	if in.Name != nil && *in.Name == "" {
		return nil, domain.ErrValidation
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		existing, err := f.items.GetByID(ctx, itemID)
		if err != nil {
			return err
		}
		if existing.RestaurantID != restaurantID {
			return domain.ErrNotFound // IDOR: item belongs to another restaurant
		}
		applyItem(existing, in)
		if err := f.items.Update(ctx, existing); err != nil {
			return err
		}
		if in.Tags != nil {
			return f.items.ReplaceTags(ctx, itemID, tagsOf(itemID, in.Tags))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f.items.GetByID(ctx, itemID)
}

func (f *facade) Delete(ctx context.Context, restaurantID, itemID uuid.UUID) error {
	return f.ownedThen(ctx, restaurantID, itemID, func(ctx context.Context) error {
		return f.items.Delete(ctx, itemID)
	})
}

func (f *facade) SetAvailable(ctx context.Context, restaurantID, itemID uuid.UUID, available bool) error {
	return f.ownedThen(ctx, restaurantID, itemID, func(ctx context.Context) error {
		return f.items.SetAvailable(ctx, itemID, available)
	})
}

// ownedThen verifies itemID belongs to restaurantID (IDOR) then runs fn.
func (f *facade) ownedThen(ctx context.Context, restaurantID, itemID uuid.UUID, fn func(context.Context) error) error {
	existing, err := f.items.GetByID(ctx, itemID)
	if err != nil {
		return err
	}
	if existing.RestaurantID != restaurantID {
		return domain.ErrNotFound
	}
	return fn(ctx)
}

func (f *facade) CreateCategory(ctx context.Context, in CategoryInput) (*domain.MenuCategory, error) {
	if in.Name == "" {
		return nil, domain.ErrValidation
	}
	c := &domain.MenuCategory{ID: uuid.New(), Name: in.Name, NameI18n: in.NameI18n, ParentID: in.ParentID, DisplayOrder: in.DisplayOrder}
	if err := f.categories.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (f *facade) UpdateCategory(ctx context.Context, id uuid.UUID, in CategoryInput) (*domain.MenuCategory, error) {
	if in.Name == "" {
		return nil, domain.ErrValidation
	}
	if in.ParentID != nil {
		if err := f.checkNoCycle(ctx, id, *in.ParentID); err != nil {
			return nil, err
		}
	}
	c := &domain.MenuCategory{ID: id, Name: in.Name, NameI18n: in.NameI18n, ParentID: in.ParentID, DisplayOrder: in.DisplayOrder}
	if err := f.categories.Update(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (f *facade) DeleteCategory(ctx context.Context, id uuid.UUID) error {
	return f.categories.Delete(ctx, id)
}

// checkNoCycle rejects assigning parentID to category id when doing so would
// make id its own ancestor — a self-reference or a longer loop — which would
// make a parent-chain traversal spin forever. It walks the existing parent
// links up from parentID looking for id, bounded by the category count so a
// pre-existing cycle can't hang the check either.
func (f *facade) checkNoCycle(ctx context.Context, id, parentID uuid.UUID) error {
	if parentID == id {
		return domain.ErrValidation
	}
	cats, err := f.categories.List(ctx)
	if err != nil {
		return err
	}
	parent := make(map[uuid.UUID]*uuid.UUID, len(cats))
	for _, c := range cats {
		parent[c.ID] = c.ParentID
	}
	cur := &parentID
	for steps := 0; cur != nil && steps <= len(cats); steps++ {
		if *cur == id {
			return domain.ErrValidation
		}
		cur = parent[*cur]
	}
	return nil
}

// applyItem copies the non-nil fields of in onto m.
func applyItem(m *domain.MenuItem, in ItemInput) {
	if in.Name != nil {
		m.Name = *in.Name
	}
	if in.NameI18n != nil {
		m.NameI18n = in.NameI18n
	}
	if in.Description != nil {
		m.Description = *in.Description
	}
	if in.DescriptionI18n != nil {
		m.DescriptionI18n = in.DescriptionI18n
	}
	if in.Price != nil {
		m.Price = *in.Price
	}
	if in.ImageURL != nil {
		m.ImageURL = in.ImageURL
	}
	if in.IsAvailable != nil {
		m.IsAvailable = *in.IsAvailable
	}
	if in.Category != nil {
		m.Category = in.Category
	}
	if in.CategoryI18n != nil {
		m.CategoryI18n = in.CategoryI18n
	}
	if in.Subcategory != nil {
		m.Subcategory = in.Subcategory
	}
	if in.SubcategoryI18n != nil {
		m.SubcategoryI18n = in.SubcategoryI18n
	}
	if in.PortionSize != nil {
		m.PortionSize = in.PortionSize
	}
	if in.PortionSizeI18n != nil {
		m.PortionSizeI18n = in.PortionSizeI18n
	}
	if in.Language != nil {
		m.Language = in.Language
	}
	if in.DisplayOrder != nil {
		m.DisplayOrder = in.DisplayOrder
	}
	if m.Price == "" {
		m.Price = "0"
	}
}

// tagsOf builds MenuItemTag rows from the input tag strings (nil → empty),
// de-duplicating so a body like ["halal","halal"] doesn't trip the
// UNIQUE(menu_item_id, tag) constraint (which would surface as a 500).
func tagsOf(itemID uuid.UUID, tags *[]string) []domain.MenuItemTag {
	if tags == nil {
		return nil
	}
	seen := make(map[string]bool, len(*tags))
	out := make([]domain.MenuItemTag, 0, len(*tags))
	for _, t := range *tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, domain.MenuItemTag{MenuItemID: itemID, Tag: t})
	}
	return out
}
