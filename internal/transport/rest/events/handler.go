// Package events exposes the public and admin HTTP endpoints for restaurant
// events. Public routes (a restaurant's published upcoming events + one event)
// need no auth and are localized via reqlocale. Admin CRUD routes mount on a
// group running middleware.Auth; the RBAC gate (PermRestaurantManage at the
// event's own restaurant) is resolved inside usecase/events, so transport only
// builds the Actor and parses ids.
package events

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/events"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Handler serves the event endpoints.
type Handler struct{ facade uc.Facade }

// NewHandler builds the events HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the unauthenticated read routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/events", h.listPublic)
	rg.GET("/restaurants/:id/events/:eventId", h.getPublic)
}

// RegisterAdminRoutes mounts the admin CRUD routes. Mount on a group running
// middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/restaurants/:id/events", h.create)
	rg.GET("/admin/restaurants/:id/events", h.listAdmin)
	rg.GET("/admin/events/:eventId", h.getAdmin)
	rg.PUT("/admin/events/:eventId", h.update)
	rg.DELETE("/admin/events/:eventId", h.delete)
}

func (h *Handler) listPublic(c *gin.Context) {
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListPublic(c.Request.Context(), rid, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]eventResponse, 0, len(items))
	for _, e := range items {
		out = append(out, publicResponse(e, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) getPublic(c *gin.Context) {
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	e, err := h.facade.GetPublic(c.Request.Context(), rid, eid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, publicResponse(*e, reqlocale.Resolve(c)))
}

func (h *Handler) create(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	var req eventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	startsAt, endsAt, ok := req.parseWindow(c)
	if !ok {
		return
	}
	e, err := h.facade.Create(c.Request.Context(), actor, uc.CreateInput{
		RestaurantID:     rid,
		Title:            req.Title,
		TitleI18n:        domain.I18n(req.TitleI18n),
		Description:      req.Description,
		DescriptionI18n:  domain.I18n(req.DescriptionI18n),
		StartsAt:         startsAt,
		EndsAt:           endsAt,
		Venue:            req.Venue,
		CoverImageURL:    req.CoverImageURL,
		Status:           domain.EventStatus(req.Status),
		Ticketed:         req.Ticketed,
		TicketPriceMinor: req.TicketPriceMinor,
		Capacity:         req.Capacity,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*e))
}

func (h *Handler) update(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	var req eventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	startsAt, endsAt, ok := req.parseWindow(c)
	if !ok {
		return
	}
	e, err := h.facade.Update(c.Request.Context(), actor, eid, uc.UpdateInput{
		Title:            req.Title,
		TitleI18n:        domain.I18n(req.TitleI18n),
		Description:      req.Description,
		DescriptionI18n:  domain.I18n(req.DescriptionI18n),
		StartsAt:         startsAt,
		EndsAt:           endsAt,
		Venue:            req.Venue,
		CoverImageURL:    req.CoverImageURL,
		Status:           domain.EventStatus(req.Status),
		Ticketed:         req.Ticketed,
		TicketPriceMinor: req.TicketPriceMinor,
		Capacity:         req.Capacity,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*e))
}

func (h *Handler) delete(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	if err := h.facade.Delete(c.Request.Context(), actor, eid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

func (h *Handler) getAdmin(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	e, err := h.facade.GetAdmin(c.Request.Context(), actor, eid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*e))
}

func (h *Handler) listAdmin(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	page, perPage := pagination(c)
	statuses := parseEventStatuses(c.Query("status"))
	items, total, err := h.facade.ListAdmin(c.Request.Context(), actor, rid, statuses, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]eventResponse, 0, len(items))
	for _, e := range items {
		out = append(out, adminResponse(e))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// --- helpers ---

func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

func pathUUID(c *gin.Context, param, msg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, msg)
		return uuid.Nil, false
	}
	return id, true
}

func pagination(c *gin.Context) (page, perPage int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

func parseEventStatuses(raw string) []domain.EventStatus {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []domain.EventStatus
	for _, part := range strings.Split(raw, ",") {
		s := domain.EventStatus(strings.TrimSpace(part))
		if s.Valid() {
			out = append(out, s)
		}
	}
	return out
}

// --- DTOs ---

type eventRequest struct {
	Title            string            `json:"title"`
	TitleI18n        map[string]string `json:"title_i18n"`
	Description      string            `json:"description"`
	DescriptionI18n  map[string]string `json:"description_i18n"`
	StartsAt         string            `json:"starts_at"`
	EndsAt           string            `json:"ends_at"`
	Venue            string            `json:"venue"`
	CoverImageURL    *string           `json:"cover_image_url"`
	Status           string            `json:"status"`
	Ticketed         bool              `json:"ticketed"`
	TicketPriceMinor *int64            `json:"ticket_price_minor"`
	Capacity         *int              `json:"capacity"`
}

// parseWindow parses starts_at/ends_at as RFC3339. On a malformed/empty value it
// writes a 422 and returns ok=false.
func (r eventRequest) parseWindow(c *gin.Context) (startsAt, endsAt time.Time, ok bool) {
	startsAt, err := time.Parse(time.RFC3339, r.StartsAt)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "starts_at must be an RFC3339 timestamp")
		return time.Time{}, time.Time{}, false
	}
	endsAt, err = time.Parse(time.RFC3339, r.EndsAt)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "ends_at must be an RFC3339 timestamp")
		return time.Time{}, time.Time{}, false
	}
	return startsAt, endsAt, true
}

type eventResponse struct {
	ID               string            `json:"id"`
	RestaurantID     string            `json:"restaurant_id"`
	Title            string            `json:"title"`
	TitleI18n        map[string]string `json:"title_i18n,omitempty"`
	Description      string            `json:"description"`
	DescriptionI18n  map[string]string `json:"description_i18n,omitempty"`
	StartsAt         string            `json:"starts_at"`
	EndsAt           string            `json:"ends_at"`
	Venue            string            `json:"venue,omitempty"`
	CoverImageURL    *string           `json:"cover_image_url,omitempty"`
	Status           string            `json:"status"`
	Ticketed         bool              `json:"ticketed"`
	TicketPriceMinor *int64            `json:"ticket_price_minor,omitempty"`
	Capacity         *int              `json:"capacity,omitempty"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
}

// adminResponse is the full staff-facing shape: base scalar + the raw i18n maps
// so the cabinet can edit translations.
func adminResponse(e domain.Event) eventResponse {
	return eventResponse{
		ID:               e.ID.String(),
		RestaurantID:     e.RestaurantID.String(),
		Title:            e.Title,
		TitleI18n:        e.TitleI18n,
		Description:      e.Description,
		DescriptionI18n:  e.DescriptionI18n,
		StartsAt:         e.StartsAt.Format(time.RFC3339),
		EndsAt:           e.EndsAt.Format(time.RFC3339),
		Venue:            e.Venue,
		CoverImageURL:    e.CoverImageURL,
		Status:           string(e.Status),
		Ticketed:         e.Ticketed,
		TicketPriceMinor: e.TicketPriceMinor,
		Capacity:         e.Capacity,
		CreatedAt:        e.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        e.UpdatedAt.Format(time.RFC3339),
	}
}

// publicResponse localizes title/description into lang and omits the raw i18n
// maps — the guest-facing shape. lang == "" leaves the base scalar untouched.
func publicResponse(e domain.Event, lang string) eventResponse {
	r := adminResponse(e)
	r.Title = e.TitleI18n.Resolve(lang, e.Title)
	r.Description = e.DescriptionI18n.Resolve(lang, e.Description)
	r.TitleI18n = nil
	r.DescriptionI18n = nil
	return r
}
