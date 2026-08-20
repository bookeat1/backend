package gastroguide

import (
	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/gastroguide"
)

// The guest-facing HTTP endpoints of «Гастропрогулки». They sit beside the
// collection endpoints, on the same public /api/v1 group, localized the same
// way, and take the same ?city= filter — a route is guide content and the home
// screen composes it exactly like a collection.
//
// They are SEPARATE endpoints rather than a shape inside
// /gastroguide/collections because a route's payload is different in kind: an
// ordered list of stops, most of which have no venue behind them. Folding it in
// would force every collection card to carry an empty points array and every
// client to branch on which of two things it just received.

// RouteHandler serves the guest route endpoints.
type RouteHandler struct{ facade uc.RouteFacade }

// NewRouteHandler builds the guest route HTTP handler.
func NewRouteHandler(f uc.RouteFacade) *RouteHandler { return &RouteHandler{facade: f} }

// RegisterPublic mounts the guest read routes.
func (h *RouteHandler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/gastroguide/routes", h.listRoutes)
	rg.GET("/gastroguide/routes/:slug", h.getRoute)
}

// listRoutes returns the live routes.
// @Summary     List published gastro routes
// @Tags        gastroguide
// @Produce     json
// @Param       city query string false "City filter (routes of that city plus the city-agnostic ones)"
// @Success     200 {object} response.Envelope
// @Failure     422 {object} response.Envelope "city_required"
// @Router      /api/v1/gastroguide/routes [get]
func (h *RouteHandler) listRoutes(c *gin.Context) {
	city, ok := optionalCity(c)
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListRoutes(c.Request.Context(), uc.RouteListInput{
		City: city, Page: page, PerPage: perPage,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]routeResponse, 0, len(items))
	for _, rt := range items {
		out = append(out, newRouteResponse(rt, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// getRoute returns one live route with its stops in order.
// @Summary     Read a published gastro route by slug
// @Tags        gastroguide
// @Produce     json
// @Success     200 {object} response.Envelope
// @Failure     404 {object} response.Envelope "unknown slug or not published"
// @Router      /api/v1/gastroguide/routes/{slug} [get]
func (h *RouteHandler) getRoute(c *gin.Context) {
	detail, err := h.facade.GetRoute(c.Request.Context(), c.Param("slug"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := routeDetailResponse{
		routeResponse: newRouteResponse(detail.GastroRoute, lang),
		Points:        make([]routePointResponse, 0, len(detail.Points)),
	}
	for _, p := range detail.Points {
		out.Points = append(out.Points, newRoutePointResponse(p, lang))
	}
	response.OK(c.Writer, out)
}

// --- DTOs ---

// routeResponse is one route card. Localized fields are already resolved for
// the caller's language; the raw *_i18n maps are not exposed on a public read.
type routeResponse struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Description is the preface to the itinerary, present on both the listing
	// and the detail.
	Description string `json:"description,omitempty"`
	// CoverImageURL is omitted when the route has no cover.
	CoverImageURL *string `json:"cover_image_url,omitempty"`
	// DurationLabel is the editor's line under the title («1 день · 4 точки»),
	// shown as written.
	DurationLabel string `json:"duration_label,omitempty"`
	// City is omitted when the route is not tied to one city.
	City string `json:"city,omitempty"`
	// Position is the editor's explicit order, exposed so a client that caches
	// pages can re-sort them itself.
	Position int `json:"position"`
	// PointCount counts EVERY stop, including the ones whose venue is currently
	// inactive: they are still part of the walk.
	PointCount int `json:"point_count"`
}

func newRouteResponse(rt domain.GastroRoute, lang string) routeResponse {
	out := routeResponse{
		ID:            rt.ID.String(),
		Slug:          rt.Slug,
		Title:         rt.TitleI18n.Resolve(lang, rt.Title),
		Description:   rt.DescriptionI18n.Resolve(lang, rt.Description),
		CoverImageURL: rt.CoverImageURL,
		DurationLabel: rt.DurationLabelI18n.Resolve(lang, rt.DurationLabel),
		Position:      rt.Position,
		PointCount:    rt.PointCount,
	}
	if rt.City != nil {
		out.City = string(*rt.City)
	}
	return out
}

type routeDetailResponse struct {
	routeResponse
	// Points is always an array and always in walking order.
	Points []routePointResponse `json:"points"`
}

// routePointResponse is one stop. Its own title/description/photo/address are
// always present as written by the editor; `venue` appears only when the stop
// points at a venue a guest can actually open right now.
type routePointResponse struct {
	ID       string `json:"id"`
	Position int    `json:"position"`
	// Kind is "restaurant" or "place" — the editorial intent. It is NOT a
	// promise that `venue` is filled: a restaurant stop whose venue is
	// deactivated comes back without one. Branch on `venue`, not on `kind`.
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	PhotoURL    *string `json:"photo_url,omitempty"`
	Address     string  `json:"address,omitempty"`
	// Latitude/Longitude are both present or both absent.
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	// Venue is the resolved catalog card — everything the client needs to draw
	// the stop and open the venue, so it never fans out one request per stop.
	// Absent for a place stop and for a venue that is currently deactivated.
	Venue *routePointVenueResponse `json:"venue,omitempty"`
}

// routePointVenueResponse is the venue card of a stop: enough to render the
// row and open the venue screen, not a copy of the whole catalog entry.
type routePointVenueResponse struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Address         string  `json:"address,omitempty"`
	CuisineType     string  `json:"cuisine_type,omitempty"`
	City            string  `json:"city"`
	PriceCategory   string  `json:"price_category,omitempty"`
	PrimaryImageURL *string `json:"primary_image_url,omitempty"`
}

func newRoutePointResponse(p domain.GuideRoutePoint, lang string) routePointResponse {
	out := routePointResponse{
		ID:          p.ID.String(),
		Position:    p.Position,
		Kind:        string(p.Kind),
		Title:       p.TitleI18n.Resolve(lang, p.Title),
		Description: p.DescriptionI18n.Resolve(lang, p.Description),
		PhotoURL:    p.PhotoURL,
		Address:     p.AddressI18n.Resolve(lang, p.Address),
		Latitude:    p.Latitude,
		Longitude:   p.Longitude,
	}
	if p.Venue != nil {
		out.Venue = &routePointVenueResponse{
			ID:              p.Venue.ID.String(),
			Name:            p.Venue.NameI18n.Resolve(lang, p.Venue.Name),
			Address:         p.Venue.AddressI18n.Resolve(lang, p.Venue.Address),
			CuisineType:     p.Venue.CuisineTypeI18n.Resolve(lang, p.Venue.CuisineType),
			City:            string(p.Venue.City),
			PriceCategory:   string(p.Venue.PriceCategory),
			PrimaryImageURL: p.Venue.PrimaryImageURL,
		}
	}
	return out
}
