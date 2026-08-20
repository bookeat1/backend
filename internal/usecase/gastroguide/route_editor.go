package gastroguide

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The editor side of «Гастропрогулки». SUPERADMIN-ONLY, for the same reason the
// collection editor is: a route is the platform's own editorial opinion about
// how to spend a day in the city, and a venue owner who could write one would
// put themselves on every walk. The gate is the same
// middleware.RequireRole(domain.RoleAdmin) at the router, re-checked here.

// RouteEditor exposes the routes' write operations plus the cabinet reads
// (which show drafts and therefore cannot come from the guest facade).
type RouteEditor interface {
	ListRoutes(ctx context.Context, actor EditorActor, in RouteAdminListInput) ([]domain.GastroRoute, int, error)
	GetRoute(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRouteAdminDetail, error)
	CreateRoute(ctx context.Context, actor EditorActor, in RouteInput) (*domain.GastroRoute, error)
	UpdateRoute(ctx context.Context, actor EditorActor, id uuid.UUID, in RouteInput) (*domain.GastroRoute, error)
	// Publish takes a route live. publishedAt nil means "now"; a future time is
	// a scheduled publication and is allowed on purpose. A route with NO stops
	// is refused — see the method comment.
	Publish(ctx context.Context, actor EditorActor, id uuid.UUID, publishedAt *time.Time) (*domain.GastroRoute, error)
	// Unpublish returns a route to draft and CLEARS published_at.
	Unpublish(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRoute, error)
	// Archive withdraws a route, keeping its stops and its publication date.
	Archive(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRoute, error)

	AddPoint(ctx context.Context, actor EditorActor, routeID uuid.UUID, in PointInput) (*domain.GuideRoutePoint, error)
	UpdatePoint(ctx context.Context, actor EditorActor, routeID, pointID uuid.UUID, in PointInput) (*domain.GuideRoutePoint, error)
	DeletePoint(ctx context.Context, actor EditorActor, routeID, pointID uuid.UUID) error
	// ReorderPoints writes the intended FINAL order of the route's stops.
	ReorderPoints(ctx context.Context, actor EditorActor, routeID uuid.UUID, pointIDs []uuid.UUID) error
}

// RouteAdminListInput narrows the cabinet's route listing.
type RouteAdminListInput struct {
	Statuses []domain.GuideRouteStatus
	City     *domain.City
	Query    string
	Page     int
	PerPage  int
}

// RouteInput is a route's editable fields as they arrive from the cabinet.
// Status is absent by design — see Publish/Unpublish/Archive.
type RouteInput struct {
	Slug              string
	Title             string
	TitleI18n         domain.I18n
	Description       string
	DescriptionI18n   domain.I18n
	CoverImageURL     *string
	DurationLabel     string
	DurationLabelI18n domain.I18n
	City              *domain.City
	Position          int
}

// PointInput is one stop as the cabinet posts it. Position is absent: a new
// stop is appended, and moving stops is ReorderPoints.
type PointInput struct {
	Kind            domain.GuideRoutePointKind
	RestaurantID    *uuid.UUID
	Title           string
	TitleI18n       domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	PhotoURL        *string
	Address         string
	AddressI18n     domain.I18n
	Latitude        *float64
	Longitude       *float64
}

type routeEditor struct {
	repo  domain.GastroRouteEditorRepository
	clock func() time.Time
}

// NewRouteEditor constructs the route editor usecase.
func NewRouteEditor(repo domain.GastroRouteEditorRepository) RouteEditor {
	return &routeEditor{repo: repo, clock: time.Now}
}

// authorizeRoute is the single superadmin gate, called FIRST by every method.
// It duplicates the router's RequireRole on purpose: the day somebody mounts
// these routes on the wrong group, the usecase still refuses.
func (e *routeEditor) authorize(a EditorActor) error {
	if a.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: the gastroguide editor is superadmin-only", domain.ErrForbidden)
	}
	return nil
}

// --- routes ---

func (e *routeEditor) ListRoutes(ctx context.Context, actor EditorActor, in RouteAdminListInput) ([]domain.GastroRoute, int, error) {
	if err := e.authorize(actor); err != nil {
		return nil, 0, err
	}
	for _, s := range in.Statuses {
		if !s.Valid() {
			return nil, 0, fmt.Errorf("%w: unknown route status %q", domain.ErrValidation, s)
		}
	}
	if in.City != nil && !in.City.Valid() {
		return nil, 0, domain.WithCode(domain.CodeCityRequired,
			fmt.Errorf("%w: unknown city", domain.ErrValidation))
	}
	return e.repo.ListRoutesAdmin(ctx, domain.GastroRouteAdminFilter{
		Statuses: in.Statuses, City: in.City, Query: in.Query,
		Page: in.Page, PerPage: in.PerPage,
	})
}

func (e *routeEditor) GetRoute(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRouteAdminDetail, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	return e.repo.GetRouteAdmin(ctx, id)
}

func (e *routeEditor) CreateRoute(ctx context.Context, actor EditorActor, in RouteInput) (*domain.GastroRoute, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateRoute(in)
	if err != nil {
		return nil, err
	}
	return e.repo.CreateRoute(ctx, w)
}

func (e *routeEditor) UpdateRoute(ctx context.Context, actor EditorActor, id uuid.UUID, in RouteInput) (*domain.GastroRoute, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateRoute(in)
	if err != nil {
		return nil, err
	}
	return e.repo.UpdateRoute(ctx, id, w)
}

// Publish takes a route live.
//
// AN EMPTY ROUTE IS REFUSED (CodeGuideRouteEmpty), and this is deliberately the
// OPPOSITE of what we decided for collections, where the same guard was removed
// in PR #81. The two objects are not the same kind of content:
//
//   - a collection is an ARTICLE. Its title, cover and text are the payload,
//     and the venues it links are a bonus. One about places that are not in our
//     catalog at all still reads perfectly, so refusing to publish it was
//     stopping editorial work for no reader-visible reason.
//   - a route IS its sequence of stops. Take the stops away and «Классический
//     тур по Алматы · 1 день · 4 точки» renders as a title, a cover, a duration
//     label that contradicts itself, and nothing to walk. There is no article
//     underneath — the description of a route is a preface to the itinerary,
//     not a substitute for it.
//
// The check counts ALL stops, not the openable ones: a stop at a deactivated
// venue and a stop at a park are both content, and counting only bookable
// venues would reintroduce exactly the mistake the collection guard made.
//
// The other precondition is published_at, which the DB CHECK requires; we
// supply `now` when the editor did not name a time. A time in the FUTURE is
// accepted — that is scheduled publication, and the guest predicate
// (published_at <= now) already implements it.
func (e *routeEditor) Publish(ctx context.Context, actor EditorActor, id uuid.UUID, publishedAt *time.Time) (*domain.GastroRoute, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	current, err := e.repo.GetRouteAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(current.Title) == "" || strings.TrimSpace(current.Slug) == "" {
		return nil, fmt.Errorf("%w: a published route needs a slug and a title", domain.ErrValidation)
	}
	n, err := e.repo.CountPoints(ctx, id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, domain.WithCode(domain.CodeGuideRouteEmpty,
			fmt.Errorf("%w: a route with no points has nothing to walk", domain.ErrValidation))
	}
	at := e.clock()
	if publishedAt != nil {
		at = *publishedAt
	}
	return e.repo.SetRouteStatus(ctx, id, domain.GuideRoutePublished, &at)
}

// Unpublish returns a route to draft and clears published_at, so a later
// re-publish gets a fresh date instead of claiming the route has been live
// since whenever it first was.
func (e *routeEditor) Unpublish(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRoute, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	return e.repo.SetRouteStatus(ctx, id, domain.GuideRouteDraft, nil)
}

// Archive withdraws a route but KEEPS published_at: an archived route is one
// that was live, and losing the date would lose that fact.
func (e *routeEditor) Archive(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GastroRoute, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	current, err := e.repo.GetRouteAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	return e.repo.SetRouteStatus(ctx, id, domain.GuideRouteArchived, current.PublishedAt)
}

// --- points ---

func (e *routeEditor) AddPoint(ctx context.Context, actor EditorActor, routeID uuid.UUID, in PointInput) (*domain.GuideRoutePoint, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validatePoint(in)
	if err != nil {
		return nil, err
	}
	return e.repo.AddPoint(ctx, routeID, w)
}

func (e *routeEditor) UpdatePoint(ctx context.Context, actor EditorActor, routeID, pointID uuid.UUID, in PointInput) (*domain.GuideRoutePoint, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validatePoint(in)
	if err != nil {
		return nil, err
	}
	return e.repo.UpdatePoint(ctx, routeID, pointID, w)
}

func (e *routeEditor) DeletePoint(ctx context.Context, actor EditorActor, routeID, pointID uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	return e.repo.DeletePoint(ctx, routeID, pointID)
}

// ReorderPoints hands the intended final order straight to the repository,
// which checks it against the current stops and writes it in one transaction.
// The usecase adds only the authorization and the cheap "obviously wrong"
// checks, so the membership comparison happens exactly once, under the row
// lock, and cannot go stale between a usecase check and the write.
func (e *routeEditor) ReorderPoints(ctx context.Context, actor EditorActor, routeID uuid.UUID, pointIDs []uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	for _, id := range pointIDs {
		if id == uuid.Nil {
			return domain.WithCode(domain.CodeGuideOrderMismatch,
				fmt.Errorf("%w: the order contains an empty point id", domain.ErrValidation))
		}
	}
	return e.repo.ReorderPoints(ctx, routeID, pointIDs)
}

// --- validation ---

func validateRoute(in RouteInput) (domain.GastroRouteWrite, error) {
	slug, err := normalizeSlug(in.Slug)
	if err != nil {
		return domain.GastroRouteWrite{}, err
	}
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return domain.GastroRouteWrite{}, err
	}
	if in.City != nil && !in.City.Valid() {
		return domain.GastroRouteWrite{}, domain.WithCode(domain.CodeCityRequired,
			fmt.Errorf("%w: unknown city", domain.ErrValidation))
	}
	return domain.GastroRouteWrite{
		Slug: slug, Title: title, TitleI18n: cleanI18n(in.TitleI18n),
		Description: strings.TrimSpace(in.Description), DescriptionI18n: cleanI18n(in.DescriptionI18n),
		CoverImageURL:     emptyToNil(in.CoverImageURL),
		DurationLabel:     strings.TrimSpace(in.DurationLabel),
		DurationLabelI18n: cleanI18n(in.DurationLabelI18n),
		City:              in.City, Position: in.Position,
	}, nil
}

// validatePoint enforces what the schema cannot, and mirrors what it can.
//
// The kind ↔ restaurant_id pairing is the important one: a 'place' stop with a
// venue is refused by the DB too, but a 'restaurant' stop WITHOUT one is not —
// the column stays nullable so that deleting a venue clears the link instead of
// deleting the stop (ON DELETE SET NULL). Which means the "a venue stop names a
// venue" rule can only live here, at the write.
func validatePoint(in PointInput) (domain.GuideRoutePointWrite, error) {
	if !in.Kind.Valid() {
		return domain.GuideRoutePointWrite{}, fmt.Errorf(
			"%w: kind must be %q or %q", domain.ErrValidation,
			domain.GuideRoutePointRestaurant, domain.GuideRoutePointPlace)
	}
	switch in.Kind {
	case domain.GuideRoutePointRestaurant:
		if in.RestaurantID == nil || *in.RestaurantID == uuid.Nil {
			return domain.GuideRoutePointWrite{}, fmt.Errorf(
				"%w: a restaurant point needs a restaurant_id", domain.ErrValidation)
		}
	case domain.GuideRoutePointPlace:
		if in.RestaurantID != nil {
			return domain.GuideRoutePointWrite{}, fmt.Errorf(
				"%w: a place point cannot reference a restaurant", domain.ErrValidation)
		}
	}
	// The stop's own headline is required even for a venue stop: the real data
	// says «Утро: Daily Coffee», which is the editorial line, not the venue's
	// name — falling back to the name would quietly drop the half that carries
	// the meaning.
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return domain.GuideRoutePointWrite{}, err
	}
	lat, lng, err := normalizeCoords(in.Latitude, in.Longitude)
	if err != nil {
		return domain.GuideRoutePointWrite{}, err
	}
	return domain.GuideRoutePointWrite{
		Kind: in.Kind, RestaurantID: in.RestaurantID,
		Title: title, TitleI18n: cleanI18n(in.TitleI18n),
		Description: strings.TrimSpace(in.Description), DescriptionI18n: cleanI18n(in.DescriptionI18n),
		PhotoURL: emptyToNil(in.PhotoURL),
		Address:  strings.TrimSpace(in.Address), AddressI18n: cleanI18n(in.AddressI18n),
		Latitude: lat, Longitude: lng,
	}, nil
}

// normalizeCoords refuses half a pair and an out-of-range value. Half a pair is
// not a rounding problem: a stop with a latitude and no longitude would be
// pinned on the prime meridian off the coast of Africa.
func normalizeCoords(lat, lng *float64) (*float64, *float64, error) {
	if (lat == nil) != (lng == nil) {
		return nil, nil, fmt.Errorf("%w: latitude and longitude must be set together", domain.ErrValidation)
	}
	if lat == nil {
		return nil, nil, nil
	}
	if *lat < -90 || *lat > 90 {
		return nil, nil, fmt.Errorf("%w: latitude must be between -90 and 90", domain.ErrValidation)
	}
	if *lng < -180 || *lng > 180 {
		return nil, nil, fmt.Errorf("%w: longitude must be between -180 and 180", domain.ErrValidation)
	}
	return lat, lng, nil
}

// emptyToNil turns a blank image URL into "there is no image". An empty string
// would be emitted by the API and make the app render a broken picture.
func emptyToNil(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
