package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The editor side of «Гастропрогулки» (migration 0078). Like the collection
// editor it is superadmin-only: a route is platform editorial content, and a
// venue owner who could write one would put themselves on every itinerary in
// the city.

// GastroRouteAdminFilter narrows the editor's route listing. Unlike
// GastroRouteFilter it can widen visibility — seeing drafts is the point of the
// cabinet.
type GastroRouteAdminFilter struct {
	// Statuses limits the listing to these publication states. Empty means all.
	Statuses []GuideRouteStatus
	// City filters by the route's OWN city column exactly (no folding in of the
	// city-agnostic rows), same posture as GuideCollectionAdminFilter.
	City *City
	// Query is a case-insensitive substring match over slug and title.
	Query   string
	Page    int
	PerPage int
}

// GastroRouteAdminDetail is a route as the EDITOR sees it: every stop,
// including the ones whose venue is currently dark — the editor has to see that
// stop 2 of 5 cannot be opened by a guest right now, or the route will look
// broken in the app for no visible reason.
type GastroRouteAdminDetail struct {
	GastroRoute
	Points []GuideRoutePoint
}

// GastroRouteWrite is the full set of a route's editable fields. Create and
// Update both take it whole: an editor form posts the entire route, and a
// partial-update protocol would make "clear the description" and "do not touch
// the description" indistinguishable.
//
// The *I18n maps arrive ALREADY MERGED — the request carries a partial patch
// (I18nPatch) and the usecase merges it onto the stored map before this struct
// is built. See GuideCollectionWrite for the full reasoning.
//
// Status and PublishedAt are NOT here — publication is its own operation with
// its own precondition, so a typo fix can never take a route live.
type GastroRouteWrite struct {
	Slug              string
	Title             string
	TitleI18n         I18n
	Description       string
	DescriptionI18n   I18n
	CoverImageURL     *string
	DurationLabel     string
	DurationLabelI18n I18n
	City              *City
	Position          int
}

// GuideRoutePointWrite is one stop's editable fields. Position is NOT here: a
// new stop is appended by the repository (append is a race when the caller
// computes the number), and moving stops is the reorder operation.
type GuideRoutePointWrite struct {
	Kind GuideRoutePointKind
	// RestaurantID is required for a restaurant stop and must be nil for a
	// place stop. The schema enforces the second half; the usecase enforces the
	// first, because the column stays nullable so a deleted venue clears the
	// link instead of deleting the stop.
	RestaurantID    *uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	PhotoURL        *string
	Address         string
	AddressI18n     I18n
	Latitude        *float64
	Longitude       *float64
}

// GastroRouteEditorRepository is the write model of the routes plus the reads
// the cabinet needs (which show drafts, and so cannot come from the guest
// model). Nothing here takes a `now`: the editor sees a route whatever its
// published_at says.
type GastroRouteEditorRepository interface {
	// ListRoutesAdmin returns routes of ANY status in editorial order,
	// paginated, plus the total.
	ListRoutesAdmin(ctx context.Context, f GastroRouteAdminFilter) ([]GastroRoute, int, error)
	// GetRouteAdmin returns one route of any status with every stop, dark
	// venues included. Unknown id is ErrNotFound.
	GetRouteAdmin(ctx context.Context, id uuid.UUID) (*GastroRouteAdminDetail, error)
	// CreateRoute inserts a route as a DRAFT. A duplicate slug is
	// ErrAlreadyExists tagged CodeGuideSlugTaken.
	CreateRoute(ctx context.Context, in GastroRouteWrite) (*GastroRoute, error)
	// UpdateRoute replaces a route's editable fields, leaving its status and
	// published_at alone.
	UpdateRoute(ctx context.Context, id uuid.UUID, in GastroRouteWrite) (*GastroRoute, error)
	// SetRouteStatus moves a route between draft/published/archived.
	// publishedAt is written as given; the DB refuses a published row without
	// one, so the usecase supplies it.
	SetRouteStatus(ctx context.Context, id uuid.UUID, status GuideRouteStatus, publishedAt *time.Time) (*GastroRoute, error)
	// CountPoints returns how many stops the route has, of any kind and
	// whatever the state of their venues. It is what publication is checked
	// against.
	CountPoints(ctx context.Context, id uuid.UUID) (int, error)

	// AddPoint appends a stop to the end of the route. An unknown route is
	// ErrNotFound; an unknown restaurant is ErrNotFound too.
	AddPoint(ctx context.Context, routeID uuid.UUID, in GuideRoutePointWrite) (*GuideRoutePoint, error)
	// UpdatePoint replaces one stop's fields, keeping its position. Unknown
	// stop (or a stop of another route) is ErrNotFound.
	UpdatePoint(ctx context.Context, routeID, pointID uuid.UUID, in GuideRoutePointWrite) (*GuideRoutePoint, error)
	// DeletePoint removes a stop and closes the gap it left, so positions stay
	// 1..N with no hole.
	DeletePoint(ctx context.Context, routeID, pointID uuid.UUID) error
	// ReorderPoints writes a whole new ordering in ONE transaction: pointIDs is
	// the intended FINAL sequence and must name exactly the route's current
	// stops, each once. Anything else is ErrValidation tagged
	// CodeGuideOrderMismatch and nothing is written.
	ReorderPoints(ctx context.Context, routeID uuid.UUID, pointIDs []uuid.UUID) error
	// ListRoutePointIDs returns the route's stops in editorial order. Used by
	// the reorder check and by tests.
	ListRoutePointIDs(ctx context.Context, routeID uuid.UUID) ([]uuid.UUID, error)
}
