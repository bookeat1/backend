package menu

import (
	"time"

	"backend-core/internal/domain"
)

type menuItemResponse struct {
	ID              string            `json:"id"`
	RestaurantID    string            `json:"restaurant_id"`
	Name            string            `json:"name"`
	NameI18n        map[string]string `json:"name_i18n,omitempty"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	Price           string            `json:"price"`
	ImageURL        *string           `json:"image_url"`
	IsAvailable     bool              `json:"is_available"`
	Category        *string           `json:"category"`
	CategoryI18n    map[string]string `json:"category_i18n,omitempty"`
	Subcategory     *string           `json:"subcategory"`
	SubcategoryI18n map[string]string `json:"subcategory_i18n,omitempty"`
	PortionSize     *string           `json:"portion_size"`
	PortionSizeI18n map[string]string `json:"portion_size_i18n,omitempty"`
	Language        *string           `json:"language"`
	DisplayOrder    *int              `json:"display_order"`
	Tags            []string          `json:"tags"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type menuCategoryResponse struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	NameI18n     map[string]string `json:"name_i18n,omitempty"`
	ParentID     *string           `json:"parent_id"`
	DisplayOrder int               `json:"display_order"`
}

func itemToResponse(m *domain.MenuItem) menuItemResponse {
	tags := make([]string, 0, len(m.Tags))
	for _, t := range m.Tags {
		tags = append(tags, t.Tag)
	}
	return menuItemResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(), Name: m.Name, NameI18n: m.NameI18n,
		Description: m.Description, DescriptionI18n: m.DescriptionI18n, Price: m.Price, ImageURL: m.ImageURL,
		IsAvailable: m.IsAvailable, Category: m.Category, CategoryI18n: m.CategoryI18n,
		Subcategory: m.Subcategory, SubcategoryI18n: m.SubcategoryI18n, PortionSize: m.PortionSize,
		PortionSizeI18n: m.PortionSizeI18n, Language: m.Language, DisplayOrder: m.DisplayOrder,
		Tags: tags, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func categoryToResponse(c domain.MenuCategory) menuCategoryResponse {
	var parent *string
	if c.ParentID != nil {
		s := c.ParentID.String()
		parent = &s
	}
	return menuCategoryResponse{ID: c.ID.String(), Name: c.Name, NameI18n: c.NameI18n, ParentID: parent, DisplayOrder: c.DisplayOrder}
}
