package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// City is a restaurant's city as STORED: the free-text VARCHAR column
// restaurants.city, whose values are the raw Supabase labels (Cyrillic).
//
// Since migration 0081 it is no longer the source of truth — CityEntry (table
// `cities`) is. This type stays because it is the backward-compatibility
// contract: a store build reads the city as a string and sends the same string
// back as ?city=, the catalog filter still compares that column, and the legacy
// importers write it on insert. Treat it as "the rendering", not "the list".
type City string

const (
	CityAstana City = "Астана"
	CityAlmaty City = "Алматы"
)

// Valid reports whether c is one of the two cities that predate the
// dictionary. It is a FALLBACK only: the authoritative check is "does this
// spelling resolve in `cities`/`city_aliases`" (CityRepository.ResolveAlias),
// which is what usecase/restaurants uses when a resolver is wired. Keeping the
// constants means a service started without the dictionary still refuses
// garbage instead of accepting anything.
func (c City) Valid() bool { return c == CityAstana || c == CityAlmaty }

// Cities lists the two cities the platform launched with, in the order the
// clients have always received them. This is the SEED of migration 0081 (same
// values, same display order) and the fallback for Valid above — the live list
// comes from CityRepository.List.
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
	ID               uuid.UUID
	CategoryID       *uuid.UUID
	Name             string
	NameI18n         I18n
	Description      string
	DescriptionI18n  I18n
	CuisineType      string
	CuisineTypeI18n  I18n
	Address          string
	AddressI18n      I18n
	OpeningHours     string
	OpeningHoursI18n I18n
	City             City
	PriceCategory    PriceCategory
	// PriceMin / PriceMax are the average-check range in WHOLE tenge, shown
	// alongside the categorical PriceCategory. Both nil (range not declared) or
	// both set with 0 <= PriceMin <= PriceMax — see migration 0068's CHECK.
	// Pointers so "not set" is distinct from a genuine 0.
	PriceMin           *int
	PriceMax           *int
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
	// Cuisines is the venue's cuisine set from the dictionary (migration
	// 0079), in the venue's own order — the first entry is its main cuisine.
	// The legacy scalar Restaurant.CuisineType is the comma-joined rendering
	// of exactly this set and stays in the payload for store builds.
	// Empty for a venue whose historical cuisine string has not been mapped
	// to the dictionary yet; CuisineType is then still populated.
	Cuisines []Cuisine
	// VenueState carries the server-computed schedule / open-now / bookability
	// facts for the public payload. Nil means "not computed" (the enrichment is
	// optional, see usecase/restaurants.WithVenueState) — the transport layer
	// then omits those JSON fields entirely rather than defaulting them.
	VenueState *PublicVenueState
}

// Paging defaults shared by every catalog read. They live here, not in the
// repository, because THREE layers have to agree on them: the repository
// (which builds LIMIT/OFFSET), the usecase (which pages a venue-state-filtered
// set in memory) and the transport layer (which echoes page/per_page back to
// the client). A private copy in each is how the echoed per_page and the
// number of rows actually returned drift apart.
const (
	DefaultPerPage = 20
	MaxPerPage     = 100
)

// NormalizePaging applies the catalog's paging defaults: a non-positive page is
// the first one, a non-positive per-page is DefaultPerPage, and per-page is
// capped at MaxPerPage.
func NormalizePaging(page, perPage int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = DefaultPerPage
	}
	if perPage > MaxPerPage {
		perPage = MaxPerPage
	}
	return page, perPage
}

// CatalogScanLimit caps an Unpaginated catalog read (see
// RestaurantFilter.Unpaginated). It is a safety ceiling on a query that has no
// LIMIT of its own, not a page size: the venue-state filter is evaluated over
// the whole matching set, and the set has to be bounded by something. The
// current catalog is two orders of magnitude below it.
const CatalogScanLimit = 2000

// RestaurantFilter narrows a listing query. Zero-value fields are ignored.
type RestaurantFilter struct {
	City      *City
	Category  *uuid.UUID
	IsPopular *bool
	IsNew     *bool
	Search    string // case-insensitive substring match on name
	Page      int    // 1-based; <=0 means 1
	PerPage   int    // <=0 means default (20), capped at 100
	// Unpaginated asks for the WHOLE matching set (up to CatalogScanLimit)
	// instead of one page, ignoring Page/PerPage. Set only by
	// usecase/restaurants when a VenueStateFilter is in play: that filter is
	// evaluated in Go, after the rows are read, so paging before it would page
	// the wrong set and report a page-local total. No transport layer sets it.
	Unpaginated bool
	// IncludeInactive lifts the `is_active = true` restriction every public
	// listing carries. ONLY the superadmin catalog screen sets it: a hidden
	// venue has to be visible to whoever hid it, or it can never be brought
	// back. Guest-facing paths must leave this false — an inactive venue is
	// unbookable, so showing one to a guest is a dead end.
	IncludeInactive bool
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
	// Unpaginated — see RestaurantFilter.Unpaginated.
	Unpaginated bool
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
	// Cuisines — see RestaurantAggregate.Cuisines. Loaded for the listing too,
	// because the app builds its cuisine chips from a catalog page.
	Cuisines []Cuisine
	// VenueState — see RestaurantAggregate.VenueState. Nil = not computed.
	VenueState *PublicVenueState
}

// RestaurantBrief is a minimal (id, localizable name) row. It backs the
// superadmin variant of the my-restaurants picker, which spans EVERY venue on
// the platform — including inactive/hidden ones, since a superadmin manages
// them all (see usecase/restaurants.MyRestaurantsUseCase).
type RestaurantBrief struct {
	ID       uuid.UUID
	Name     string
	NameI18n I18n
}
