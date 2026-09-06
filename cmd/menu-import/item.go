// Command menu-import upserts a parsed menu JSON file into the existing
// menu_items/menu_categories tables for one restaurant. See doc.go for the
// full contract; this file holds the input DTO and its mapping to domain.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"backend-core/internal/domain"
)

// ParsedItem is one dish as it appears in a parsed menu JSON file (see the
// `<venue>-menu-*/menu.json` recon files). Field names/casing vary slightly
// between venues' parsers (source_image vs image_url, price as an int major
// unit, optional name_en/page/price_alt), so every field here is tolerant of
// being absent.
type ParsedItem struct {
	Section     string  `json:"section"`
	Page        any     `json:"page,omitempty"` // string or number depending on parser; unused, kept only to accept the field
	Name        string  `json:"name"`
	NameEN      *string `json:"name_en,omitempty"`
	Description *string `json:"description,omitempty"`
	Price       *int64  `json:"price,omitempty"` // integer, WHOLE tenge (major units), never minor units
	Unit        *string `json:"unit,omitempty"`
	ImageURL    *string `json:"image_url,omitempty"`
	SourceImage *string `json:"source_image,omitempty"`
}

// ParseMenuFile decodes a parsed-menu JSON array into ParsedItem rows.
func ParseMenuFile(data []byte) ([]ParsedItem, error) {
	var items []ParsedItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("decode menu json: %w", err)
	}
	return items, nil
}

// NormalizeName trims whitespace and lower-cases a dish name for matching. It
// is NOT applied to the stored Name column — only to the comparison key.
func NormalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizeSection trims whitespace off a section/category label. Unlike the
// name key it keeps case: category names are shown to staff as-is.
func normalizeSection(s string) string {
	return strings.TrimSpace(s)
}

// priceString renders a parsed item's price as the decimal string the domain
// expects ("4500.00"). A nil/negative price is rejected by the caller before
// this is used — see ToDomainFields.
func priceString(p int64) string {
	return strconv.FormatInt(p, 10) + ".00"
}

// ToDomainFields copies the parsed row's data onto the mutable fields of an
// existing/new domain.MenuItem (Name, Description, Price, Category,
// PortionSize, image, i18n). It never touches ID/RestaurantID/IsAvailable/
// IsFeatured/TopPickPosition/CreatedAt — those are either identity, or
// venue-managed state the importer must not clobber (see doc.go).
func (p ParsedItem) ToDomainFields(m *domain.MenuItem) error {
	if p.Price == nil {
		return fmt.Errorf("%w: item %q has no price", domain.ErrValidation, p.Name)
	}
	if *p.Price < 0 {
		return fmt.Errorf("%w: item %q has negative price %d", domain.ErrValidation, p.Name, *p.Price)
	}
	m.Name = strings.TrimSpace(p.Name)
	if p.Description != nil {
		m.Description = strings.TrimSpace(*p.Description)
	}
	m.Price = priceString(*p.Price)
	if sec := normalizeSection(p.Section); sec != "" {
		m.Category = &sec
	} else {
		m.Category = nil
	}
	if p.Unit != nil && strings.TrimSpace(*p.Unit) != "" {
		u := strings.TrimSpace(*p.Unit)
		m.PortionSize = &u
	} else {
		m.PortionSize = nil
	}
	img := firstNonEmpty(p.ImageURL, p.SourceImage)
	m.ImageURL = img
	if p.NameEN != nil && strings.TrimSpace(*p.NameEN) != "" {
		m.NameI18n = domain.I18n{domain.LocaleEN: strings.TrimSpace(*p.NameEN)}
	} else {
		m.NameI18n = nil
	}
	return nil
}

func firstNonEmpty(vals ...*string) *string {
	for _, v := range vals {
		if v != nil && strings.TrimSpace(*v) != "" {
			s := strings.TrimSpace(*v)
			return &s
		}
	}
	return nil
}
