package menu

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/menu"
)

type menuItemRequest struct {
	Name            *string           `json:"name"`
	NameI18n        map[string]string `json:"name_i18n"`
	Description     *string           `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n"`
	Price           *string           `json:"price"`
	ImageURL        *string           `json:"image_url"`
	IsAvailable     *bool             `json:"is_available"`
	Category        *string           `json:"category"`
	CategoryI18n    map[string]string `json:"category_i18n"`
	Subcategory     *string           `json:"subcategory"`
	SubcategoryI18n map[string]string `json:"subcategory_i18n"`
	PortionSize     *string           `json:"portion_size"`
	PortionSizeI18n map[string]string `json:"portion_size_i18n"`
	// Language labels the row's own text. The only values a write accepts are
	// null and "ru": a dish row IS the base row, translations go into the
	// *_i18n maps. Anything else is refused with code
	// menu_item_language_not_base — see checkBaseLanguage in usecase/menu.
	Language     *string   `json:"language"`
	DisplayOrder *int      `json:"display_order"`
	Tags         *[]string `json:"tags"`
}

func (r menuItemRequest) toInput() uc.ItemInput {
	return uc.ItemInput{
		Name: r.Name, NameI18n: domain.I18n(r.NameI18n), Description: r.Description,
		DescriptionI18n: domain.I18n(r.DescriptionI18n), Price: r.Price, ImageURL: r.ImageURL,
		IsAvailable: r.IsAvailable, Category: r.Category, CategoryI18n: domain.I18n(r.CategoryI18n),
		Subcategory: r.Subcategory, SubcategoryI18n: domain.I18n(r.SubcategoryI18n),
		PortionSize: r.PortionSize, PortionSizeI18n: domain.I18n(r.PortionSizeI18n),
		Language: r.Language, DisplayOrder: r.DisplayOrder, Tags: r.Tags,
	}
}

type availabilityRequest struct {
	IsAvailable bool `json:"is_available"`
}

type featuredRequest struct {
	IsFeatured bool `json:"is_featured"`
}

// topPickRequest is the body of PATCH .../menu-items/:itemId/top-pick.
type topPickRequest struct {
	IsTopPick bool `json:"is_top_pick"`
}

// topPicksOrderRequest is the body of PUT /restaurants/:id/menu-highlights:
// the venue's whole rail, in order. An empty (but present) list clears it.
type topPicksOrderRequest struct {
	ItemIDs []string `json:"item_ids"`
}

// toUUIDs parses the ordered ids, keeping the order. A malformed id is a 422
// for the whole request: partially applying an ordering nobody asked for would
// leave the rail in a state the panel never described.
func (r topPicksOrderRequest) toUUIDs() ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(r.ItemIDs))
	for _, raw := range r.ItemIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid item id %q", domain.ErrValidation, raw)
		}
		out = append(out, id)
	}
	return out, nil
}

type menuCategoryRequest struct {
	Name         string            `json:"name"`
	NameI18n     map[string]string `json:"name_i18n"`
	ParentID     *string           `json:"parent_id"`
	DisplayOrder int               `json:"display_order"`
}

func (r menuCategoryRequest) toInput() (uc.CategoryInput, error) {
	in := uc.CategoryInput{Name: r.Name, NameI18n: domain.I18n(r.NameI18n), DisplayOrder: r.DisplayOrder}
	if r.ParentID != nil {
		id, err := uuid.Parse(*r.ParentID)
		if err != nil {
			return uc.CategoryInput{}, fmt.Errorf("invalid parent_id: %w", domain.ErrValidation)
		}
		in.ParentID = &id
	}
	return in, nil
}
