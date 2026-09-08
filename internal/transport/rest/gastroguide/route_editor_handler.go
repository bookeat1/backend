package gastroguide

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/gastroguide"
)

// The cabinet's route endpoints, all under /api/v1/admin/gastroguide/routes and
// all SUPERADMIN-ONLY — mounted on the same group as the collection editor
// (which already runs middleware.RequireRole(domain.RoleAdmin)), with the
// usecase re-checking the role.
//
// As with the collections, the admin DTOs are NOT the guest ones: an editor
// gets the base ru value AND the raw *_i18n map, because they are editing the
// translations, and gets every stop including the ones whose venue is dark.

// RouteEditorHandler serves the gastro-route editor endpoints.
type RouteEditorHandler struct{ editor uc.RouteEditor }

// NewRouteEditorHandler builds the route editor HTTP handler.
func NewRouteEditorHandler(e uc.RouteEditor) *RouteEditorHandler {
	return &RouteEditorHandler{editor: e}
}

// RegisterAdminRoutes mounts the route editor. The provided group MUST already
// enforce middleware.RequireRole(domain.RoleAdmin).
//
// The point-scoped sub-resources sit under /routes/:id/points/… , the same
// shape the collection's venue routes use.
func (h *RouteEditorHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/gastroguide/routes", h.listRoutes)
	rg.POST("/admin/gastroguide/routes", h.createRoute)
	rg.GET("/admin/gastroguide/routes/:id", h.getRoute)
	rg.PUT("/admin/gastroguide/routes/:id", h.updateRoute)
	rg.POST("/admin/gastroguide/routes/:id/publish", h.publish)
	rg.POST("/admin/gastroguide/routes/:id/unpublish", h.unpublish)
	rg.POST("/admin/gastroguide/routes/:id/archive", h.archive)
	rg.POST("/admin/gastroguide/routes/:id/points", h.addPoint)
	rg.PUT("/admin/gastroguide/routes/:id/points/order", h.reorderPoints)
	rg.PUT("/admin/gastroguide/routes/:id/points/:pointId", h.updatePoint)
	rg.DELETE("/admin/gastroguide/routes/:id/points/:pointId", h.deletePoint)
}

func (h *RouteEditorHandler) actor(c *gin.Context) (uc.EditorActor, bool) {
	return editorActor(c)
}

// --- routes ---

// listRoutes returns routes of any status.
// @Summary     List gastro routes (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Router      /api/v1/admin/gastroguide/routes [get]
func (h *RouteEditorHandler) listRoutes(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var statuses []domain.GuideRouteStatus
	for _, raw := range strings.Split(c.Query("status"), ",") {
		if s := strings.TrimSpace(raw); s != "" {
			statuses = append(statuses, domain.GuideRouteStatus(s))
		}
	}
	var city *domain.City
	if raw := strings.TrimSpace(c.Query("city")); raw != "" {
		v := domain.City(raw)
		city = &v
	}
	page, perPage := pagination(c)
	items, total, err := h.editor.ListRoutes(c.Request.Context(), actor, uc.RouteAdminListInput{
		Statuses: statuses, City: city, Query: strings.TrimSpace(c.Query("q")),
		Page: page, PerPage: perPage,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]adminRouteResponse, 0, len(items))
	for _, rt := range items {
		out = append(out, newAdminRouteResponse(rt))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// getRoute returns one route with every stop.
// @Summary     Read a gastro route (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id} [get]
func (h *RouteEditorHandler) getRoute(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	detail, err := h.editor.GetRoute(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminRouteDetail(*detail))
}

// createRoute adds a route as a draft.
// @Summary     Create a gastro route (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     201 {object} response.Envelope
// @Failure     409 {object} response.Envelope "guide_slug_taken"
// @Router      /api/v1/admin/gastroguide/routes [post]
func (h *RouteEditorHandler) createRoute(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req routeRequest
	if !bindJSON(c, &req) {
		return
	}
	rt, err := h.editor.CreateRoute(c.Request.Context(), actor, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, newAdminRouteResponse(*rt))
}

// updateRoute replaces a route's editable fields.
// @Summary     Update a gastro route (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id} [put]
func (h *RouteEditorHandler) updateRoute(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	var req routeRequest
	if !bindJSON(c, &req) {
		return
	}
	rt, err := h.editor.UpdateRoute(c.Request.Context(), actor, id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminRouteResponse(*rt))
}

// publish takes a route live. A route with no stops is refused with
// guide_route_empty.
// @Summary     Publish a gastro route (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     422 {object} response.Envelope "guide_route_empty"
// @Router      /api/v1/admin/gastroguide/routes/{id}/publish [post]
func (h *RouteEditorHandler) publish(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	// An empty body is the normal case ("publish now"); only a client asking
	// for a scheduled publication sends published_at.
	var req publishRequest
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &req) {
			return
		}
	}
	rt, err := h.editor.Publish(c.Request.Context(), actor, id, req.PublishedAt)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminRouteResponse(*rt))
}

// unpublish returns a route to draft.
// @Summary     Unpublish a gastro route (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id}/unpublish [post]
func (h *RouteEditorHandler) unpublish(c *gin.Context) {
	h.statusChange(c, func(ctx *gin.Context, actor uc.EditorActor, id uuid.UUID) (*domain.GastroRoute, error) {
		return h.editor.Unpublish(ctx.Request.Context(), actor, id)
	})
}

// archive withdraws a route.
// @Summary     Archive a gastro route (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id}/archive [post]
func (h *RouteEditorHandler) archive(c *gin.Context) {
	h.statusChange(c, func(ctx *gin.Context, actor uc.EditorActor, id uuid.UUID) (*domain.GastroRoute, error) {
		return h.editor.Archive(ctx.Request.Context(), actor, id)
	})
}

func (h *RouteEditorHandler) statusChange(c *gin.Context, fn func(*gin.Context, uc.EditorActor, uuid.UUID) (*domain.GastroRoute, error)) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	rt, err := fn(c, actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminRouteResponse(*rt))
}

// --- points ---

// addPoint appends a stop to a route.
// @Summary     Add a stop to a gastro route (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     201 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id}/points [post]
func (h *RouteEditorHandler) addPoint(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	var req routePointRequest
	if !bindJSON(c, &req) {
		return
	}
	in, ok := req.toInput(c)
	if !ok {
		return
	}
	p, err := h.editor.AddPoint(c.Request.Context(), actor, id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, newAdminRoutePointResponse(*p))
}

// updatePoint replaces one stop's fields, keeping its position.
// @Summary     Update a stop of a gastro route (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/routes/{id}/points/{pointId} [put]
func (h *RouteEditorHandler) updatePoint(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	pointID, ok := pathUUID(c, "pointId", "invalid point id")
	if !ok {
		return
	}
	var req routePointRequest
	if !bindJSON(c, &req) {
		return
	}
	in, ok := req.toInput(c)
	if !ok {
		return
	}
	p, err := h.editor.UpdatePoint(c.Request.Context(), actor, id, pointID, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminRoutePointResponse(*p))
}

// deletePoint removes a stop and closes the gap.
// @Summary     Delete a stop of a gastro route (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Router      /api/v1/admin/gastroguide/routes/{id}/points/{pointId} [delete]
func (h *RouteEditorHandler) deletePoint(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	pointID, ok := pathUUID(c, "pointId", "invalid point id")
	if !ok {
		return
	}
	if err := h.editor.DeletePoint(c.Request.Context(), actor, id, pointID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// reorderPoints writes the intended FINAL order of a route's stops. Same
// contract as the collection reorder: the body carries the whole sequence, so
// the call is idempotent and a stale screen is refused outright (422
// guide_order_mismatch) instead of silently rewriting the itinerary.
// @Summary     Reorder a gastro route's stops (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Failure     422 {object} response.Envelope "guide_order_mismatch"
// @Router      /api/v1/admin/gastroguide/routes/{id}/points/order [put]
func (h *RouteEditorHandler) reorderPoints(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid route id")
	if !ok {
		return
	}
	var req reorderPointsRequest
	if !bindJSON(c, &req) {
		return
	}
	ids, ok := parseUUIDs(c, req.PointIDs, "invalid point id")
	if !ok {
		return
	}
	if err := h.editor.ReorderPoints(c.Request.Context(), actor, id, ids); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- request DTOs ---

type routeRequest struct {
	Slug              string             `json:"slug"`
	Title             string             `json:"title"`
	TitleI18n         map[string]*string `json:"title_i18n"`
	Description       string             `json:"description"`
	DescriptionI18n   map[string]*string `json:"description_i18n"`
	CoverImageURL     *string            `json:"cover_image_url"`
	DurationLabel     string             `json:"duration_label"`
	DurationLabelI18n map[string]*string `json:"duration_label_i18n"`
	City              *string            `json:"city"`
	Position          int                `json:"position"`
}

func (r routeRequest) toInput() uc.RouteInput {
	in := uc.RouteInput{
		Slug: r.Slug, Title: r.Title, TitleI18n: domain.I18nPatch(r.TitleI18n),
		Description: r.Description, DescriptionI18n: domain.I18nPatch(r.DescriptionI18n),
		CoverImageURL:     r.CoverImageURL,
		DurationLabel:     r.DurationLabel,
		DurationLabelI18n: domain.I18nPatch(r.DurationLabelI18n),
		Position:          r.Position,
	}
	// An empty city string is "every city", the same thing a missing field
	// means: a <select> with nothing chosen posts "".
	if r.City != nil {
		if v := strings.TrimSpace(*r.City); v != "" {
			city := domain.City(v)
			in.City = &city
		}
	}
	return in
}

type routePointRequest struct {
	Kind string `json:"kind"`
	// RestaurantID is required for a restaurant stop and must be absent (or
	// empty) for a place stop.
	RestaurantID    string             `json:"restaurant_id"`
	Title           string             `json:"title"`
	TitleI18n       map[string]*string `json:"title_i18n"`
	Description     string             `json:"description"`
	DescriptionI18n map[string]*string `json:"description_i18n"`
	PhotoURL        *string            `json:"photo_url"`
	Address         string             `json:"address"`
	AddressI18n     map[string]*string `json:"address_i18n"`
	Latitude        *float64           `json:"latitude"`
	Longitude       *float64           `json:"longitude"`
}

func (r routePointRequest) toInput(c *gin.Context) (uc.PointInput, bool) {
	in := uc.PointInput{
		Kind:  domain.GuideRoutePointKind(strings.TrimSpace(r.Kind)),
		Title: r.Title, TitleI18n: domain.I18nPatch(r.TitleI18n),
		Description: r.Description, DescriptionI18n: domain.I18nPatch(r.DescriptionI18n),
		PhotoURL: r.PhotoURL,
		Address:  r.Address, AddressI18n: domain.I18nPatch(r.AddressI18n),
		Latitude: r.Latitude, Longitude: r.Longitude,
	}
	if raw := strings.TrimSpace(r.RestaurantID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(c.Writer, http.StatusBadRequest, "invalid restaurant id")
			return uc.PointInput{}, false
		}
		in.RestaurantID = &id
	}
	return in, true
}

type reorderPointsRequest struct {
	// PointIDs is the intended FINAL order, complete.
	PointIDs []string `json:"point_ids"`
}

// --- response DTOs ---

type adminRouteResponse struct {
	ID                string      `json:"id"`
	Slug              string      `json:"slug"`
	Title             string      `json:"title"`
	TitleI18n         domain.I18n `json:"title_i18n,omitempty"`
	Description       string      `json:"description"`
	DescriptionI18n   domain.I18n `json:"description_i18n,omitempty"`
	CoverImageURL     *string     `json:"cover_image_url"`
	DurationLabel     string      `json:"duration_label"`
	DurationLabelI18n domain.I18n `json:"duration_label_i18n,omitempty"`
	City              *string     `json:"city"`
	Status            string      `json:"status"`
	PublishedAt       *time.Time  `json:"published_at"`
	Position          int         `json:"position"`
	PointCount        int         `json:"point_count"`
	UpdatedAt         time.Time   `json:"updated_at"`
}

func newAdminRouteResponse(rt domain.GastroRoute) adminRouteResponse {
	out := adminRouteResponse{
		ID: rt.ID.String(), Slug: rt.Slug,
		Title: rt.Title, TitleI18n: rt.TitleI18n,
		Description: rt.Description, DescriptionI18n: rt.DescriptionI18n,
		CoverImageURL:     rt.CoverImageURL,
		DurationLabel:     rt.DurationLabel,
		DurationLabelI18n: rt.DurationLabelI18n,
		Status:            string(rt.Status), PublishedAt: rt.PublishedAt,
		Position: rt.Position, PointCount: rt.PointCount, UpdatedAt: rt.UpdatedAt,
	}
	if rt.City != nil {
		v := string(*rt.City)
		out.City = &v
	}
	return out
}

type adminRouteDetailResponse struct {
	adminRouteResponse
	Points []adminRoutePointResponse `json:"points"`
}

func newAdminRouteDetail(d domain.GastroRouteAdminDetail) adminRouteDetailResponse {
	out := adminRouteDetailResponse{
		adminRouteResponse: newAdminRouteResponse(d.GastroRoute),
		Points:             make([]adminRoutePointResponse, 0, len(d.Points)),
	}
	for _, p := range d.Points {
		out.Points = append(out.Points, newAdminRoutePointResponse(p))
	}
	return out
}

// adminRoutePointResponse is a stop in the cabinet. It carries the raw *_i18n
// maps (the editor writes translations) and the venue's is_active flag, which
// the guest response does not have and the editor cannot work without: it is
// why stop 2 of an itinerary shows no venue card in the app.
type adminRoutePointResponse struct {
	ID              string      `json:"id"`
	Position        int         `json:"position"`
	Kind            string      `json:"kind"`
	RestaurantID    *string     `json:"restaurant_id"`
	Title           string      `json:"title"`
	TitleI18n       domain.I18n `json:"title_i18n,omitempty"`
	Description     string      `json:"description"`
	DescriptionI18n domain.I18n `json:"description_i18n,omitempty"`
	PhotoURL        *string     `json:"photo_url"`
	Address         string      `json:"address"`
	AddressI18n     domain.I18n `json:"address_i18n,omitempty"`
	Latitude        *float64    `json:"latitude"`
	Longitude       *float64    `json:"longitude"`
	// Venue is present for any stop that still links a venue row, active or
	// not; IsActive says which.
	Venue *adminRoutePointVenueResponse `json:"venue"`
}

type adminRoutePointVenueResponse struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Address         string  `json:"address"`
	CuisineType     string  `json:"cuisine_type"`
	City            string  `json:"city"`
	PriceCategory   string  `json:"price_category"`
	PrimaryImageURL *string `json:"primary_image_url"`
	IsActive        bool    `json:"is_active"`
}

func newAdminRoutePointResponse(p domain.GuideRoutePoint) adminRoutePointResponse {
	out := adminRoutePointResponse{
		ID: p.ID.String(), Position: p.Position, Kind: string(p.Kind),
		Title: p.Title, TitleI18n: p.TitleI18n,
		Description: p.Description, DescriptionI18n: p.DescriptionI18n,
		PhotoURL: p.PhotoURL,
		Address:  p.Address, AddressI18n: p.AddressI18n,
		Latitude: p.Latitude, Longitude: p.Longitude,
	}
	if p.RestaurantID != nil {
		v := p.RestaurantID.String()
		out.RestaurantID = &v
	}
	if p.Venue != nil {
		out.Venue = &adminRoutePointVenueResponse{
			ID: p.Venue.ID.String(), Name: p.Venue.Name, Address: p.Venue.Address,
			CuisineType: p.Venue.CuisineType, City: string(p.Venue.City),
			PriceCategory:   string(p.Venue.PriceCategory),
			PrimaryImageURL: p.Venue.PrimaryImageURL,
			IsActive:        p.Venue.IsActive,
		}
	}
	return out
}
