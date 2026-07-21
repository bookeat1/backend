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

// RestaurantRepository persists restaurants. Get* return ErrNotFound when absent.
type RestaurantRepository interface {
	Create(ctx context.Context, r *Restaurant) error
	Update(ctx context.Context, r *Restaurant) error
	GetByID(ctx context.Context, id uuid.UUID) (*RestaurantAggregate, error)
	// ListActive returns active restaurants matching f plus the total count.
	// Ordering: display_order (NULLs last), then name. PrimaryImage is populated.
	ListActive(ctx context.Context, f RestaurantFilter) ([]RestaurantListItem, int, error)
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
}

// RestaurantListItem is a lightweight row for the catalog listing.
type RestaurantListItem struct {
	Restaurant
	PrimaryImage *string
}
