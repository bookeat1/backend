package domain

import (
	"context"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// priceRe matches a non-negative decimal with up to 2 fractional digits.
var priceRe = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)

// ValidPrice reports whether s is a well-formed price string (e.g. "4500.00").
func ValidPrice(s string) bool { return priceRe.MatchString(s) }

// MenuItem is a dish on a restaurant's menu. Price is a decimal string
// ("4500.00"). Category/Subcategory are free text (not FKs). Language is set
// only for multilingual menus (nil = default/ru).
type MenuItem struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	Name            string
	NameI18n        I18n
	Description     string
	DescriptionI18n I18n
	Price           string
	ImageURL        *string
	IsAvailable     bool
	Category        *string
	CategoryI18n    I18n
	Subcategory     *string
	SubcategoryI18n I18n
	PortionSize     *string
	PortionSizeI18n I18n
	Language        *string
	DisplayOrder    *int
	Tags            []MenuItemTag
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MenuItemTag is a free-text tag attached to a menu item.
type MenuItemTag struct {
	ID         uuid.UUID
	MenuItemID uuid.UUID
	Tag        string
	CreatedAt  time.Time
}

// MenuItemFilter narrows a menu listing. Language nil means the default
// language chain (ru or NULL).
type MenuItemFilter struct {
	RestaurantID uuid.UUID
	Language     *string
}

// MenuItemRepository persists menu items. Get* return ErrNotFound when absent.
type MenuItemRepository interface {
	// ListByRestaurant returns items (with Tags) for f.RestaurantID, ordered by
	// display_order (NULLs last), then name. When f.Language is nil, items with
	// language 'ru' OR NULL are returned; otherwise items with that language.
	ListByRestaurant(ctx context.Context, f MenuItemFilter) ([]MenuItem, error)
	GetByID(ctx context.Context, id uuid.UUID) (*MenuItem, error)
	Create(ctx context.Context, m *MenuItem) error
	Update(ctx context.Context, m *MenuItem) error
	Delete(ctx context.Context, id uuid.UUID) error
	SetAvailable(ctx context.Context, id uuid.UUID, available bool) error
	// ReplaceTags deletes the item's tags and inserts items (call within a tx).
	ReplaceTags(ctx context.Context, menuItemID uuid.UUID, tags []MenuItemTag) error
}
