package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// «Гастропрогулки» — the gastroguide's routes (migration 0078).
//
// A route is an ORDERED ITINERARY, which is why it is not a collection: its
// stops are not all venues («Парк 28 панфиловцев» has no catalog row), each
// stop carries its own headline («Утро: Daily Coffee») rather than the venue's
// name, and a stop only means something in its place in the sequence. The
// publication axis, on the other hand, is deliberately identical to a
// collection's — see GuideRouteStatus.

// GuideRouteStatus is a route's publication state. It is an ALIAS of
// GuideCollectionStatus, not a copy: the two are the same three states with the
// same meaning and the same guest predicate (published AND published_at <= now),
// and a second enum would be one refactor away from drifting from the first.
type GuideRouteStatus = GuideCollectionStatus

const (
	// GuideRouteDraft is being prepared. Invisible to guests.
	GuideRouteDraft = GuideCollectionDraft
	// GuideRoutePublished is live from PublishedAt on (a future PublishedAt is
	// a scheduled publication, not a live route).
	GuideRoutePublished = GuideCollectionPublished
	// GuideRouteArchived was live once and has been withdrawn, keeping its
	// points and its publication date.
	GuideRouteArchived = GuideCollectionArchived
)

// GuideRoutePointKind says what a stop IS. It is the editorial intent and
// nothing else: a 'restaurant' stop whose venue is deactivated (or whose
// catalog row was deleted) is still a 'restaurant' stop, it just comes back
// without a venue card.
type GuideRoutePointKind string

const (
	// GuideRoutePointRestaurant is a stop at a venue from our catalog. Created
	// with a RestaurantID; the guest response carries the venue card when that
	// venue is currently active.
	GuideRoutePointRestaurant GuideRoutePointKind = "restaurant"
	// GuideRoutePointPlace is a stop that has no catalog row at all (a park, a
	// bazaar, a viewpoint) and never will. It carries only its own editorial
	// content.
	GuideRoutePointPlace GuideRoutePointKind = "place"
)

// Valid reports whether k is a known point kind.
func (k GuideRoutePointKind) Valid() bool {
	switch k {
	case GuideRoutePointRestaurant, GuideRoutePointPlace:
		return true
	}
	return false
}

// GastroRoute is one route without its stops — the card shape.
type GastroRoute struct {
	ID              uuid.UUID
	Slug            string
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	// CoverImageURL is the full public image URL, or nil when the route has no
	// cover. Never a placeholder: nil means "there is no image".
	CoverImageURL *string
	// DurationLabel is the editor's own line under the title («1 день · 4
	// точки»). It is not computed from PointCount: the first half of it
	// («1 день») is a human judgement.
	DurationLabel     string
	DurationLabelI18n I18n
	// City nil means the route is shown in every city.
	City        *City
	Status      GuideRouteStatus
	PublishedAt *time.Time
	Position    int
	// PointCount is how many stops the route has, ALL of them — unlike a
	// collection's VenueCount, which counts only what a guest can open. A stop
	// whose venue went dark is still a stop the guest walks past, and hiding it
	// from the count would contradict the duration label.
	PointCount int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// GuideRoutePointVenue is the catalog part of a venue stop: enough to draw the
// card and open the venue, resolved server-side so the client never fans out
// one request per stop.
type GuideRoutePointVenue struct {
	ID              uuid.UUID
	Name            string
	NameI18n        I18n
	Address         string
	AddressI18n     I18n
	CuisineType     string
	CuisineTypeI18n I18n
	City            City
	PriceCategory   PriceCategory
	// PrimaryImageURL is the venue's primary catalog image, nil when it has none.
	PrimaryImageURL *string
	// IsActive is the venue's catalog state. On a GUEST read the venue is
	// present only when it is active, so this is always true there; the EDITOR
	// read returns dark venues too, flagged, exactly like the collection editor.
	IsActive bool
}

// GuideRoutePoint is one stop of a route: its own editorial content plus, for a
// venue stop, the resolved venue card.
type GuideRoutePoint struct {
	ID              uuid.UUID
	Position        int
	Kind            GuideRoutePointKind
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	// PhotoURL is the stop's own photo, nil when it has none.
	PhotoURL    *string
	Address     string
	AddressI18n I18n
	// Latitude/Longitude are both set or both nil — the schema enforces the
	// pair, so half a coordinate cannot put a pin on the equator.
	Latitude  *float64
	Longitude *float64
	// RestaurantID is the venue this stop points at, nil for a place stop (and
	// for a venue stop whose catalog row was deleted — the link is cleared, the
	// stop stays).
	RestaurantID *uuid.UUID
	// Venue is the resolved catalog card. Nil when the stop is a place, when
	// its venue was deleted, or — on a guest read — when its venue is currently
	// deactivated: a card that cannot be opened or booked is a dead end, and
	// the stop reads perfectly well as its own text.
	Venue *GuideRoutePointVenue
}

// GastroRouteDetail is a route together with its stops in order.
type GastroRouteDetail struct {
	GastroRoute
	// Points are in editorial order (position, then id as the stable
	// tie-break). A published route always has at least one — publication
	// refuses an empty route, because a route IS its points.
	Points []GuideRoutePoint
}

// GastroRouteFilter narrows the public route listing. The filter cannot WIDEN
// visibility: the published-and-live rule lives in SQL.
type GastroRouteFilter struct {
	// City selects routes pinned to that city plus the city-agnostic ones
	// (city IS NULL), exactly like the collection listing. Nil means no filter.
	City    *City
	Page    int
	PerPage int
}

// GastroRouteRepository is the guest-facing read model of the routes. Every
// method takes `now` because visibility is time-dependent and the clock belongs
// to the usecase, not to the SQL.
type GastroRouteRepository interface {
	// ListPublishedRoutes returns live routes in editorial order, paginated,
	// plus the total.
	ListPublishedRoutes(ctx context.Context, f GastroRouteFilter, now time.Time) ([]GastroRoute, int, error)
	// GetPublishedRouteBySlug returns a live route with its ordered stops.
	// Returns ErrNotFound when the slug is unknown OR the route is not live — a
	// draft must not be distinguishable from a typo, or the slug of an
	// unannounced route leaks.
	GetPublishedRouteBySlug(ctx context.Context, slug string, now time.Time) (*GastroRouteDetail, error)
}
