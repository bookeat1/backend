package menu

import (
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
	Language        *string           `json:"language"`
	DisplayOrder    *int              `json:"display_order"`
	Tags            *[]string         `json:"tags"`
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

type menuCategoryRequest struct {
	Name         string            `json:"name"`
	NameI18n     map[string]string `json:"name_i18n"`
	ParentID     *string           `json:"parent_id"`
	DisplayOrder int               `json:"display_order"`
}

func (r menuCategoryRequest) toInput() uc.CategoryInput {
	in := uc.CategoryInput{Name: r.Name, NameI18n: domain.I18n(r.NameI18n), DisplayOrder: r.DisplayOrder}
	if r.ParentID != nil {
		if id, err := uuid.Parse(*r.ParentID); err == nil {
			in.ParentID = &id
		}
	}
	return in
}
