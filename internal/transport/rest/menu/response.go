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
	// PriceMinor is the SAME price in integer minor units (тиыны), so a client
	// can compute «добавить · итого» without parsing a display string back into
	// money. It is additive: Price stays exactly as it was, because the mobile
	// app reads it today.
	//
	// Pointer, and null when the stored decimal cannot be converted: `0` would
	// read as "free", and inventing an amount is worse than admitting we have
	// none. In practice menu_items.price is numeric(12,2) NOT NULL CHECK >= 0,
	// so it always converts. The authority on what a guest actually pays stays
	// the server (see PriceStringToMinor in the pre-order flow) — this field is
	// for arithmetic the UI shows, not for an amount the client sends back.
	PriceMinor  *int64  `json:"price_minor"`
	ImageURL    *string `json:"image_url"`
	IsAvailable bool    `json:"is_available"`
	IsFeatured  bool    `json:"is_featured"`
	// IsTopPick / TopPickPosition — the VENUE's own «Лучшие позиции» mark on its
	// storefront rail. Not to be confused with IsFeatured, which is the
	// cross-venue "chef's picks" rail of the main screen.
	IsTopPick       bool              `json:"is_top_pick"`
	TopPickPosition *int              `json:"top_pick_position"`
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

// itemToResponse maps one dish. lang is the resolved caller locale ("" when the
// caller asked for nothing, and for the cabinet views, which edit base text).
//
// Both models are served at once, on purpose: the scalar fields are resolved
// HERE, like every other localized entity (restaurants, events, guide), while
// the raw *_i18n maps stay in the payload because the mobile app resolves the
// menu itself today. Dropping the maps would break installed builds; not
// resolving the scalars leaves every other client with Russian text.
func itemToResponse(m *domain.MenuItem, lang string) menuItemResponse {
	tags := make([]string, 0, len(m.Tags))
	for _, t := range m.Tags {
		tags = append(tags, t.Tag)
	}
	return menuItemResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(),
		Name: m.NameI18n.Resolve(lang, m.Name), NameI18n: m.NameI18n,
		Description: m.DescriptionI18n.Resolve(lang, m.Description), DescriptionI18n: m.DescriptionI18n,
		Price:      m.Price,
		PriceMinor: priceMinorOf(m.Price), ImageURL: m.ImageURL,
		IsAvailable: m.IsAvailable, IsFeatured: m.IsFeatured,
		IsTopPick: m.TopPickPosition != nil, TopPickPosition: m.TopPickPosition,
		Category: resolvePtr(m.CategoryI18n, lang, m.Category), CategoryI18n: m.CategoryI18n,
		Subcategory: resolvePtr(m.SubcategoryI18n, lang, m.Subcategory), SubcategoryI18n: m.SubcategoryI18n,
		PortionSize:     resolvePtr(m.PortionSizeI18n, lang, m.PortionSize),
		PortionSizeI18n: m.PortionSizeI18n, Language: m.Language, DisplayOrder: m.DisplayOrder,
		Tags: tags, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// resolvePtr is I18n.Resolve for a nullable column: a NULL base stays NULL even
// when a translation exists, because the field is absent for this dish, not
// untranslated. A resolved value is returned in a NEW pointer so the domain
// aggregate is never aliased.
func resolvePtr(i domain.I18n, lang string, base *string) *string {
	if base == nil {
		return nil
	}
	v := i.Resolve(lang, *base)
	return &v
}

func categoryToResponse(c domain.MenuCategory, lang string) menuCategoryResponse {
	var parent *string
	if c.ParentID != nil {
		s := c.ParentID.String()
		parent = &s
	}
	return menuCategoryResponse{ID: c.ID.String(), Name: c.NameI18n.Resolve(lang, c.Name), NameI18n: c.NameI18n, ParentID: parent, DisplayOrder: c.DisplayOrder}
}

// featuredItemResponse is one card of the cross-venue "chef's picks" rail. It
// carries the venue name alongside the dish so the card needs no second call,
// and it deliberately reuses menuItemResponse rather than defining a parallel
// dish shape the clients would have to map twice.
type featuredItemResponse struct {
	menuItemResponse
	RestaurantName     string            `json:"restaurant_name"`
	RestaurantNameI18n map[string]string `json:"restaurant_name_i18n,omitempty"`
}

func featuredToResponse(f domain.FeaturedMenuItem, lang string) featuredItemResponse {
	item := f.Item
	return featuredItemResponse{
		menuItemResponse:   itemToResponse(&item, lang),
		RestaurantName:     f.RestaurantI18n.Resolve(lang, f.RestaurantName),
		RestaurantNameI18n: f.RestaurantI18n,
	}
}

// priceMinorOf converts the stored decimal price string into integer minor
// units through the domain's exact converter (no float round-trip). A price the
// converter refuses becomes null rather than 0: the response says "we cannot
// give you a number" instead of "this dish is free".
func priceMinorOf(price string) *int64 {
	minor, err := domain.PriceStringToMinor(price)
	if err != nil {
		return nil
	}
	return &minor
}

// itemsToResponse maps a slice of dishes, preserving order — the rail's order
// IS the payload here, so no map/sort may happen after the usecase decided it.
func itemsToResponse(items []domain.MenuItem, lang string) []menuItemResponse {
	out := make([]menuItemResponse, 0, len(items))
	for i := range items {
		out = append(out, itemToResponse(&items[i], lang))
	}
	return out
}
