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
	// The cross-venue listing and one promo's own page, addressed without a
	// restaurant — the only public reads that can show a PLATFORM promo.
	// Mirrors GET /events and GET /events/:eventId.
	rg.GET("/promos", h.listPublicActive)
	rg.GET("/promos/:promoId", h.getPublicDetail)
}

// RegisterAdminRoutes mounts the admin CRUD routes. Mount on a group running
// middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/restaurants/:id/promos", h.create)
	rg.GET("/admin/restaurants/:id/promos", h.listAdmin)
	// The PLATFORM's own акции: same payload, no restaurant in the path. Edit,
	// read and delete keep using /admin/promos/:promoId — those resolve the
	// promo first and authorize against whoever owns it.
	rg.POST("/admin/platform/promos", h.createPlatform)
	rg.GET("/admin/platform/promos", h.listPlatformAdmin)
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
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	h.createWithHost(c, &rid)
}

// createPlatform creates a promo with NO venue. The route carries no restaurant
// id, so there is nothing a caller could send to claim one; the platform-content
// policy is applied in the usecase, from the nil owner.
func (h *Handler) createPlatform(c *gin.Context) { h.createWithHost(c, nil) }

func (h *Handler) createWithHost(c *gin.Context, rid *uuid.UUID) {
	actor, ok := actorFrom(c)
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
		City:            req.City,
		Title:           req.Title,
		TitleI18n:       domain.I18n(req.TitleI18n),
		Description:     req.Description,
		DescriptionI18n: domain.I18n(req.DescriptionI18n),
		StartsAt:        startsAt,
		EndsAt:          endsAt,
		Terms:           req.Terms,
		CoverImageURL:   req.CoverImageURL,
		DiscountPercent: req.DiscountPercent,
		Status:          domain.PromoStatus(req.Status),
		Images:          req.Images,
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
		CoverImageURL:   req.CoverImageURL,
		DiscountPercent: req.DiscountPercent,
		Status:          domain.PromoStatus(req.Status),
		Images:          req.Images,
		City:            req.City,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*p))
}

// listPublicActive serves the cross-venue guest listing:
// GET /promos?city=&restaurant_id=&page=&per_page=&lang=.
func (h *Handler) listPublicActive(c *gin.Context) {
	flt, ok := publicListFilter(c)
	if !ok {
		return
	}
	items, total, err := h.facade.ListPublicActive(c.Request.Context(), flt)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]promoListItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, publicListItemResponse(it, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, flt.Page, flt.PerPage))
}

// getPublicDetail serves one promo's own page: GET /promos/{promoId}, venue
// block included only when the promo has a venue.
func (h *Handler) getPublicDetail(c *gin.Context) {
	pid, ok := pathUUID(c, "promoId", "invalid promo id")
	if !ok {
		return
	}
	it, err := h.facade.GetPublicDetail(c.Request.Context(), pid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, publicListItemResponse(*it, reqlocale.Resolve(c)))
}

// publicListFilter parses the cross-venue listing's query parameters. A
// malformed restaurant_id is 422 rather than ignored — silently dropping it
// would hand the caller the WHOLE platform's promos under one venue's name.
// An unknown city is left as it is and simply matches nothing, same as the
// events listing.
func publicListFilter(c *gin.Context) (domain.PublicPromoFilter, bool) {
	var f domain.PublicPromoFilter
	if v := strings.TrimSpace(c.Query("city")); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	if v := strings.TrimSpace(c.Query("restaurant_id")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "restaurant_id must be a uuid")
			return f, false
		}
		f.RestaurantID = &id
	}
	f.Page, f.PerPage = pagination(c)
	return f, true
}

func (h *Handler) listPlatformAdmin(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	page, perPage := pagination(c)
	statuses := parsePromoStatuses(c.Query("status"))
	items, total, err := h.facade.ListPlatformAdmin(c.Request.Context(), actor, statuses, page, perPage)
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
	// CoverImageURL is the full public image URL. Omitted or null means the
	// promo has no picture — that is a valid, honest state, not a missing field.
	CoverImageURL *string `json:"cover_image_url"`
	// DiscountPercent is the «−30%» badge value, 0..100. Omitted or null means
	// the promo has no discount badge — a valid state, validated in the usecase.
	DiscountPercent *int   `json:"discount_percent"`
	Status          string `json:"status"`
	// Images — галерея акции БЕЗ обложки; полная замена, пустой список очищает.
	Images []string `json:"images"`
	// City переопределяет город показа акции. Пусто или отсутствует — обычный
	// случай: акция живёт в городе своего заведения (а акция платформы — во
	// всех городах). Значение резолвится по справочнику городов; неизвестный
	// или скрытый город — 422. Поле, как и всё здесь, — полная замена.
	City *string `json:"city"`
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
	ID string `json:"id"`
	// RestaurantID — заведение. ОТСУТСТВУЕТ у акции платформы: заведения нет,
	// и слать сюда нули значило бы предложить клиенту открыть пустую карточку.
	RestaurantID    *string           `json:"restaurant_id,omitempty"`
	Title           string            `json:"title"`
	TitleI18n       map[string]string `json:"title_i18n,omitempty"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	StartsAt        string            `json:"starts_at"`
	EndsAt          string            `json:"ends_at"`
	Terms           string            `json:"terms,omitempty"`
	// CoverImageURL is omitted entirely when the promo has no picture: the
	// client must render its own placeholder, never a made-up URL.
	CoverImageURL *string `json:"cover_image_url,omitempty"`
	// DiscountPercent is omitted when the promo carries no discount badge: the
	// client renders no «−N%» badge, never a made-up 0%.
	DiscountPercent *int   `json:"discount_percent,omitempty"`
	Status          string `json:"status"`
	// Images — дополнительные фотографии акции, без обложки. Всегда массив.
	Images []string `json:"images"`
	// City — переопределение города. Отсутствует, когда его нет: акция тогда
	// показывается в городе заведения, а у акции платформы — во всех городах.
	City      *string `json:"city,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// promoListItemResponse is the cross-venue listing's item: the same guest-facing
// promo shape plus the venue that runs it. Restaurant is ABSENT for a platform
// promo — a client must treat that as a real, drawable state.
type promoListItemResponse struct {
	promoResponse
	Restaurant *promoRestaurantResponse `json:"restaurant,omitempty"`
}

// promoRestaurantResponse is the minimal venue identity on a promo card, the
// same shape the events listing uses.
type promoRestaurantResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	City string `json:"city"`
}

func publicListItemResponse(it domain.PromoListItem, lang string) promoListItemResponse {
	out := promoListItemResponse{promoResponse: publicResponse(it.Promo, lang)}
	if it.Restaurant != nil {
		out.Restaurant = &promoRestaurantResponse{
			ID:   it.Restaurant.ID.String(),
			Name: it.Restaurant.NameI18n.Resolve(lang, it.Restaurant.Name),
			City: string(it.Restaurant.City),
		}
	}
	return out
}

// cityOrNil renders the optional city override as an optional string: nil stays
// nil and the field is omitted — "this promo has no city of its own".
func cityOrNil(c *domain.City) *string {
	if c == nil {
		return nil
	}
	s := string(*c)
	return &s
}

// idOrNil renders an optional uuid as an optional string.
func idOrNil(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func adminResponse(p domain.Promo) promoResponse {
	return promoResponse{
		ID:              p.ID.String(),
		RestaurantID:    idOrNil(p.RestaurantID),
		Title:           p.Title,
		TitleI18n:       p.TitleI18n,
		Description:     p.Description,
		DescriptionI18n: p.DescriptionI18n,
		StartsAt:        p.StartsAt.Format(time.RFC3339),
		EndsAt:          p.EndsAt.Format(time.RFC3339),
		Terms:           p.Terms,
		CoverImageURL:   p.CoverImageURL,
		DiscountPercent: p.DiscountPercent,
		Status:          string(p.Status),
		Images:          imagesOrEmpty(p.Images),
		City:            cityOrNil(p.City),
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

// imagesOrEmpty гарантирует, что галерея в JSON — массив, а не null: клиент
// рисует ленту и null пришлось бы отдельно защищать в каждом месте.
func imagesOrEmpty(urls []string) []string {
	if urls == nil {
		return []string{}
	}
	return urls
}
