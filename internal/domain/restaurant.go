package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// City is a restaurant's city, stored as VARCHAR. Values are the raw Supabase
// enum labels (Cyrillic).
type City string

const (
	CityAstana City = "Астана"
	CityAlmaty City = "Алматы"
)

// Valid reports whether c is a known city.
func (c City) Valid() bool { return c == CityAstana || c == CityAlmaty }

// Cities lists every known city enum value, in a stable display order. There
// is no separate cities table (spec: reuse the existing city column/enum
// values, do not reinvent) — this is the single source of truth for a
// "which cities are available" endpoint.
func Cities() []City { return []City{CityAstana, CityAlmaty} }

// PriceCategory is a restaurant's price tier, stored as VARCHAR.
type PriceCategory string

const (
	PriceLow  PriceCategory = "₸"
	PriceMid  PriceCategory = "₸₸"
	PriceHigh PriceCategory = "₸₸₸"
)

// Valid reports whether p is a known price category.
func (p PriceCategory) Valid() bool {
	return p == PriceLow || p == PriceMid || p == PriceHigh
}

// I18n is a localized field of shape {"ru":...,"kk":...,"en":...}. Nil when the
// column is NULL.
type I18n map[string]string

// SupportedLocales lists the language codes the catalog can serve translated
// text in. ru is the permanent default (the base scalar columns, e.g. `name`,
// are themselves Russian text) — see LocaleRU.
var SupportedLocales = []string{"ru", "kk", "en"}

const (
	LocaleRU = "ru"
	LocaleKK = "kk"
	LocaleEN = "en"
)

// IsSupportedLocale reports whether lang is one of SupportedLocales.
func IsSupportedLocale(lang string) bool {
	for _, l := range SupportedLocales {
		if l == lang {
			return true
		}
	}
	return false
}

// Resolve returns i[lang] when it exists and is non-empty, otherwise falls
// back to base. Never invents a translation: base is always the value
// actually stored in the plain (non-i18n) column. An empty lang or a nil map
// (column was NULL) both fall back to base directly.
func (i I18n) Resolve(lang, base string) string {
	if lang == "" || i == nil {
		return base
	}
	if v, ok := i[lang]; ok && v != "" {
		return v
	}
	return base
}

// Restaurant is a venue in the catalog. ID equals the original Supabase id.
type Restaurant struct {
	ID                 uuid.UUID
	CategoryID         *uuid.UUID
	Name               string
	NameI18n           I18n
	Description        string
	DescriptionI18n    I18n
	CuisineType        string
	CuisineTypeI18n    I18n
	Address            string
	AddressI18n        I18n
	OpeningHours       string
	OpeningHoursI18n   I18n
	City               City
	PriceCategory      PriceCategory
	Email              string
	Phone              string
	Latitude           *float64
	Longitude          *float64
	KwaakaRestaurantID *string
	IsActive           bool
	IsNew              *bool
	IsPopular          *bool
	IsPremium          *bool
	HiddenFromHome     bool
	DisplayOrder       *int
	// BookingPolicy holds the venue's optional overrides of the global booking
	// policy (Wave 3). Nil fields fall back to the BOOKING_DEFAULT_* env values;
	// resolution lives in usecase/bookings.
	BookingPolicy BookingPolicyOverride
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RestaurantAggregate is a restaurant with its inline collections, matching the
// nested read the app performs on the detail screen.
type RestaurantAggregate struct {
	Restaurant
	Images      []Image
	Features    []Feature
	Tags        []Tag
	SocialLinks []SocialLink
}

// RestaurantFilter narrows a listing query. Zero-value fields are ignored.
type RestaurantFilter struct {
	City      *City
	Category  *uuid.UUID
	IsPopular *bool
	IsNew     *bool
	Search    string // case-insensitive substring match on name
	Page      int    // 1-based; <=0 means 1
	PerPage   int    // <=0 means default (20), capped at 100
}

// RestaurantSearchFilter narrows a full-text restaurant search. The zero value
// (empty Query, no filters) lists every active restaurant, ordered like the
// catalog listing. Only active restaurants are ever returned.
type RestaurantSearchFilter struct {
	// Query is free text matched against the venue's name + description across
	// ALL locales (base ru columns plus every *_i18n translation). Empty Query
	// means "no text constraint" — the search degrades to a filtered browse.
	Query string
	// City, Cuisines and Price are AND-combined with the text query. Cuisines is
	// an OR-set (cuisine_type IN (...)); an empty/nil slice means "any cuisine".
	City     *City
	Cuisines []string
	Price    *PriceCategory
	Page     int // 1-based; <=0 means 1
	PerPage  int // <=0 means default (20), capped at 100
}

// RestaurantRepository persists restaurants. Get* return ErrNotFound when absent.
type RestaurantRepository interface {
	Create(ctx context.Context, r *Restaurant) error
	Update(ctx context.Context, r *Restaurant) error
	GetByID(ctx context.Context, id uuid.UUID) (*RestaurantAggregate, error)
	// ListActive returns active restaurants matching f plus the total count.
	// Ordering: display_order (NULLs last), then name. PrimaryImage is populated.
	ListActive(ctx context.Context, f RestaurantFilter) ([]RestaurantListItem, int, error)
	// Search returns active restaurants matching f's text query and filters plus
	// the total count. When f.Query is non-empty, results are ranked by full-text
	// relevance then trigram word-similarity, with a deterministic id tie-break
	// so pagination is stable; when it is empty, ordering matches ListActive.
	Search(ctx context.Context, f RestaurantSearchFilter) ([]RestaurantListItem, int, error)
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
	// UpdateBookingPolicy patches the venue's booking-policy overrides: only
	// the non-nil fields of o are written, every other column keeps its current
	// value (a NULL stays NULL, i.e. "use the global default"). Returns
	// ErrNotFound when the restaurant does not exist.
	UpdateBookingPolicy(ctx context.Context, id uuid.UUID, o BookingPolicyOverride) error
}

// RestaurantListItem is a lightweight row for the catalog listing.
type RestaurantListItem struct {
	Restaurant
	PrimaryImage *string
}
