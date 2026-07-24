// Package promos exposes the public and admin HTTP endpoints for restaurant
// promos. Public route (a restaurant's active promos) needs no auth and is
// localized via reqlocale. Admin CRUD routes mount on a group running
// middleware.Auth; the RBAC gate (PermRestaurantManage at the promo's own
// restaurant) is resolved inside usecase/promos.
package promos

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
	uc "backend-core/internal/usecase/promos"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Handler serves the promo endpoints.
type Handler struct{ facade uc.Facade }

// NewHandler builds the promos HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the unauthenticated read route.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/promos", h.listPublic)
}

// RegisterAdminRoutes mounts the admin CRUD routes. Mount on a group running
// middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/restaurants/:id/promos", h.create)
	rg.GET("/admin/restaurants/:id/promos", h.listAdmin)
	rg.GET("/admin/promos/:promoId", h.getAdmin)
	rg.PUT("/admin/promos/:promoId", h.update)
	rg.DELETE("/admin/promos/:promoId", h.delete)
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
	out := make([]promoResponse, 0, len(items))
	for _, p := range items {
		out = append(out, publicResponse(p, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
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
	var req promoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	startsAt, endsAt, ok := req.parseWindow(c)
	if !ok {
		return
	}
	p, err := h.facade.Create(c.Request.Context(), actor, uc.CreateInput{
		RestaurantID:    rid,
		Title:           req.Title,
		TitleI18n:       domain.I18n(req.TitleI18n),
		Description:     req.Description,
		DescriptionI18n: domain.I18n(req.DescriptionI18n),
		StartsAt:        startsAt,
		EndsAt:          endsAt,
		Terms:           req.Terms,
		Status:          domain.PromoStatus(req.Status),
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*p))
}

func (h *Handler) update(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	pid, ok := pathUUID(c, "promoId", "invalid promo id")
	if !ok {
		return
	}
	var req promoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	startsAt, endsAt, ok := req.parseWindow(c)
	if !ok {
		return
	}
	p, err := h.facade.Update(c.Request.Context(), actor, pid, uc.UpdateInput{
		Title:           req.Title,
		TitleI18n:       domain.I18n(req.TitleI18n),
		Description:     req.Description,
		DescriptionI18n: domain.I18n(req.DescriptionI18n),
		StartsAt:        startsAt,
		EndsAt:          endsAt,
		Terms:           req.Terms,
		Status:          domain.PromoStatus(req.Status),
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*p))
}

func (h *Handler) delete(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	pid, ok := pathUUID(c, "promoId", "invalid promo id")
	if !ok {
		return
	}
	if err := h.facade.Delete(c.Request.Context(), actor, pid); err != nil {
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
	pid, ok := pathUUID(c, "promoId", "invalid promo id")
	if !ok {
		return
	}
	p, err := h.facade.GetAdmin(c.Request.Context(), actor, pid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*p))
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
	statuses := parsePromoStatuses(c.Query("status"))
	items, total, err := h.facade.ListAdmin(c.Request.Context(), actor, rid, statuses, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]promoResponse, 0, len(items))
	for _, p := range items {
		out = append(out, adminResponse(p))
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

func parsePromoStatuses(raw string) []domain.PromoStatus {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []domain.PromoStatus
	for _, part := range strings.Split(raw, ",") {
		s := domain.PromoStatus(strings.TrimSpace(part))
		if s.Valid() {
			out = append(out, s)
		}
	}
	return out
}

// --- DTOs ---

type promoRequest struct {
	Title           string            `json:"title"`
	TitleI18n       map[string]string `json:"title_i18n"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n"`
	StartsAt        string            `json:"starts_at"`
	EndsAt          string            `json:"ends_at"`
	Terms           string            `json:"terms"`
	Status          string            `json:"status"`
}

func (r promoRequest) parseWindow(c *gin.Context) (startsAt, endsAt time.Time, ok bool) {
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

type promoResponse struct {
	ID              string            `json:"id"`
	RestaurantID    string            `json:"restaurant_id"`
	Title           string            `json:"title"`
	TitleI18n       map[string]string `json:"title_i18n,omitempty"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	StartsAt        string            `json:"starts_at"`
	EndsAt          string            `json:"ends_at"`
	Terms           string            `json:"terms,omitempty"`
	Status          string            `json:"status"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at"`
}

func adminResponse(p domain.Promo) promoResponse {
	return promoResponse{
		ID:              p.ID.String(),
		RestaurantID:    p.RestaurantID.String(),
		Title:           p.Title,
		TitleI18n:       p.TitleI18n,
		Description:     p.Description,
		DescriptionI18n: p.DescriptionI18n,
		StartsAt:        p.StartsAt.Format(time.RFC3339),
		EndsAt:          p.EndsAt.Format(time.RFC3339),
		Terms:           p.Terms,
		Status:          string(p.Status),
		CreatedAt:       p.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       p.UpdatedAt.Format(time.RFC3339),
	}
}

func publicResponse(p domain.Promo, lang string) promoResponse {
	r := adminResponse(p)
	r.Title = p.TitleI18n.Resolve(lang, p.Title)
	r.Description = p.DescriptionI18n.Resolve(lang, p.Description)
	r.TitleI18n = nil
	r.DescriptionI18n = nil
	return r
}
