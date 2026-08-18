package gastroguide

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/gastroguide"
)

// The editor cabinet's routes, all under /api/v1/admin/gastroguide and all
// SUPERADMIN-ONLY. They are mounted on the group that already runs
// middleware.RequireRole(domain.RoleAdmin) (the same group the payout-generation
// and feed-moderation endpoints use); the usecase re-checks the role.
//
// The admin DTOs are deliberately NOT the guest ones. A guest gets one resolved
// string per field, because the app renders it; an editor gets the base ru value
// AND the raw *_i18n map, because they are editing the translations. Reusing the
// guest DTO here would make it impossible to fill in a kk title through the
// panel at all.

// EditorHandler serves the gastroguide editor endpoints.
type EditorHandler struct{ editor uc.Editor }

// NewEditorHandler builds the editor HTTP handler.
func NewEditorHandler(e uc.Editor) *EditorHandler { return &EditorHandler{editor: e} }

// RegisterAdminRoutes mounts the editor cabinet. The provided group MUST already
// enforce middleware.RequireRole(domain.RoleAdmin).
//
// Route shape: the collection-scoped sub-resources sit under
// /admin/gastroguide/collections/:id/… so a static segment and a wildcard never
// become siblings in the router tree (same rule the feed's admin routes follow).
func (h *EditorHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/gastroguide/categories", h.listCategories)
	rg.POST("/admin/gastroguide/categories", h.createCategory)
	rg.PUT("/admin/gastroguide/categories/:id", h.updateCategory)

	rg.GET("/admin/gastroguide/collections", h.listCollections)
	rg.POST("/admin/gastroguide/collections", h.createCollection)
	rg.GET("/admin/gastroguide/collections/:id", h.getCollection)
	rg.PUT("/admin/gastroguide/collections/:id", h.updateCollection)
	rg.POST("/admin/gastroguide/collections/:id/publish", h.publish)
	rg.POST("/admin/gastroguide/collections/:id/unpublish", h.unpublish)
	rg.POST("/admin/gastroguide/collections/:id/archive", h.archive)
	rg.PUT("/admin/gastroguide/collections/:id/categories", h.setCategories)
	rg.POST("/admin/gastroguide/collections/:id/venues", h.attachVenue)
	rg.PUT("/admin/gastroguide/collections/:id/venues/order", h.reorderVenues)
	rg.PUT("/admin/gastroguide/collections/:id/venues/:restaurantId/note", h.setVenueNote)
	rg.PUT("/admin/gastroguide/collections/:id/venues/:restaurantId/highlight", h.setVenueHighlight)
	rg.DELETE("/admin/gastroguide/collections/:id/venues/:restaurantId", h.detachVenue)
}

func (h *EditorHandler) actor(c *gin.Context) (uc.EditorActor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.EditorActor{}, false
	}
	return uc.EditorActor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

// --- categories ---

// listCategories returns every rubric, active or not.
// @Summary     List gastroguide rubrics (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Router      /api/v1/admin/gastroguide/categories [get]
func (h *EditorHandler) listCategories(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	items, err := h.editor.ListCategories(c.Request.Context(), actor)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]adminCategoryResponse, 0, len(items))
	for _, cat := range items {
		out = append(out, newAdminCategoryResponse(cat))
	}
	response.OK(c.Writer, gin.H{"items": out})
}

// createCategory adds a rubric.
// @Summary     Create a gastroguide rubric (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     201 {object} response.Envelope
// @Failure     409 {object} response.Envelope "guide_slug_taken"
// @Router      /api/v1/admin/gastroguide/categories [post]
func (h *EditorHandler) createCategory(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req categoryRequest
	if !bindJSON(c, &req) {
		return
	}
	cat, err := h.editor.CreateCategory(c.Request.Context(), actor, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, newAdminCategoryResponse(*cat))
}

// updateCategory replaces a rubric's fields.
// @Summary     Update a gastroguide rubric (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/categories/{id} [put]
func (h *EditorHandler) updateCategory(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid category id")
	if !ok {
		return
	}
	var req categoryRequest
	if !bindJSON(c, &req) {
		return
	}
	cat, err := h.editor.UpdateCategory(c.Request.Context(), actor, id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminCategoryResponse(*cat))
}

// --- collections ---

// listCollections returns collections of any status.
// @Summary     List gastroguide collections (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections [get]
func (h *EditorHandler) listCollections(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var statuses []domain.GuideCollectionStatus
	for _, raw := range strings.Split(c.Query("status"), ",") {
		if s := strings.TrimSpace(raw); s != "" {
			statuses = append(statuses, domain.GuideCollectionStatus(s))
		}
	}
	var city *domain.City
	if raw := strings.TrimSpace(c.Query("city")); raw != "" {
		v := domain.City(raw)
		city = &v
	}
	page, perPage := pagination(c)
	items, total, err := h.editor.ListCollections(c.Request.Context(), actor, uc.AdminListInput{
		Statuses: statuses, City: city, Query: strings.TrimSpace(c.Query("q")),
		Page: page, PerPage: perPage,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]adminCollectionResponse, 0, len(items))
	for _, col := range items {
		out = append(out, newAdminCollectionResponse(col))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// getCollection returns one collection with every venue and rubric.
// @Summary     Read a gastroguide collection (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections/{id} [get]
func (h *EditorHandler) getCollection(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	detail, err := h.editor.GetCollection(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminCollectionDetail(*detail))
}

// createCollection adds a collection as a draft.
// @Summary     Create a gastroguide collection (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     201 {object} response.Envelope
// @Failure     409 {object} response.Envelope "guide_slug_taken"
// @Router      /api/v1/admin/gastroguide/collections [post]
func (h *EditorHandler) createCollection(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req collectionRequest
	if !bindJSON(c, &req) {
		return
	}
	col, err := h.editor.CreateCollection(c.Request.Context(), actor, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, newAdminCollectionResponse(*col))
}

// updateCollection replaces a collection's editable fields.
// @Summary     Update a gastroguide collection (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections/{id} [put]
func (h *EditorHandler) updateCollection(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	var req collectionRequest
	if !bindJSON(c, &req) {
		return
	}
	col, err := h.editor.UpdateCollection(c.Request.Context(), actor, id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminCollectionResponse(*col))
}

// publish takes a collection live.
// @Summary     Publish a gastroguide collection (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections/{id}/publish [post]
func (h *EditorHandler) publish(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	// An empty body is the normal case ("publish now"); only a client that wants
	// a scheduled publication sends published_at.
	var req publishRequest
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &req) {
			return
		}
	}
	col, err := h.editor.Publish(c.Request.Context(), actor, id, req.PublishedAt)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminCollectionResponse(*col))
}

// unpublish returns a collection to draft.
// @Summary     Unpublish a gastroguide collection (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections/{id}/unpublish [post]
func (h *EditorHandler) unpublish(c *gin.Context) {
	h.statusChange(c, func(ctx *gin.Context, actor uc.EditorActor, id uuid.UUID) (*domain.GuideCollection, error) {
		return h.editor.Unpublish(ctx.Request.Context(), actor, id)
	})
}

// archive withdraws a collection.
// @Summary     Archive a gastroguide collection (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Router      /api/v1/admin/gastroguide/collections/{id}/archive [post]
func (h *EditorHandler) archive(c *gin.Context) {
	h.statusChange(c, func(ctx *gin.Context, actor uc.EditorActor, id uuid.UUID) (*domain.GuideCollection, error) {
		return h.editor.Archive(ctx.Request.Context(), actor, id)
	})
}

func (h *EditorHandler) statusChange(c *gin.Context, fn func(*gin.Context, uc.EditorActor, uuid.UUID) (*domain.GuideCollection, error)) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	col, err := fn(c, actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newAdminCollectionResponse(*col))
}

// --- membership ---

// setCategories replaces a collection's whole rubric set.
// @Summary     Set a collection's rubrics (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Router      /api/v1/admin/gastroguide/collections/{id}/categories [put]
func (h *EditorHandler) setCategories(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	var req setCategoriesRequest
	if !bindJSON(c, &req) {
		return
	}
	ids, ok := parseUUIDs(c, req.CategoryIDs, "invalid category id")
	if !ok {
		return
	}
	if err := h.editor.SetCategories(c.Request.Context(), actor, id, ids); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// attachVenue appends a venue to a collection.
// @Summary     Attach a venue to a collection (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Failure     409 {object} response.Envelope "guide_venue_already_attached"
// @Router      /api/v1/admin/gastroguide/collections/{id}/venues [post]
func (h *EditorHandler) attachVenue(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	var req attachVenueRequest
	if !bindJSON(c, &req) {
		return
	}
	rid, err := uuid.Parse(strings.TrimSpace(req.RestaurantID))
	if err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid restaurant id")
		return
	}
	eventID, promoID, ok := parseHighlight(c, req.EventID, req.PromoID)
	if !ok {
		return
	}
	if err := h.editor.AttachVenue(c.Request.Context(), actor, id, uc.AttachVenueInput{
		RestaurantID: rid, Note: req.Note, NoteI18n: req.NoteI18n,
		EventID: eventID, PromoID: promoID,
	}); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// detachVenue removes a venue from a collection.
// @Summary     Detach a venue from a collection (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Router      /api/v1/admin/gastroguide/collections/{id}/venues/{restaurantId} [delete]
func (h *EditorHandler) detachVenue(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "restaurantId", "invalid restaurant id")
	if !ok {
		return
	}
	if err := h.editor.DetachVenue(c.Request.Context(), actor, id, rid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// setVenueNote rewrites the editor's line under one venue.
// @Summary     Set a venue's note inside a collection (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Router      /api/v1/admin/gastroguide/collections/{id}/venues/{restaurantId}/note [put]
func (h *EditorHandler) setVenueNote(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "restaurantId", "invalid restaurant id")
	if !ok {
		return
	}
	var req venueNoteRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.editor.SetVenueNote(c.Request.Context(), actor, id, rid, req.Note, req.NoteI18n); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// reorderVenues writes the intended FINAL order of a collection's venues.
//
// The body carries the whole sequence, not a move: "restaurant_ids" is what the
// collection must look like when the call returns. A move-based protocol (swap
// A and B, insert X at 3) is a sequence of writes that can half-apply and whose
// meaning depends on what the server currently holds — with drag-and-drop on the
// other end, that is a curation silently scrambled by a lost request. Sending
// the final order makes the operation idempotent (replaying it changes nothing)
// and lets the server refuse a stale payload outright: if the list does not name
// exactly the current members, the answer is 422 guide_order_mismatch and
// nothing is written.
//
// @Summary     Reorder a collection's venues (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Failure     422 {object} response.Envelope "guide_order_mismatch"
// @Router      /api/v1/admin/gastroguide/collections/{id}/venues/order [put]
func (h *EditorHandler) reorderVenues(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	var req reorderRequest
	if !bindJSON(c, &req) {
		return
	}
	ids, ok := parseUUIDs(c, req.RestaurantIDs, "invalid restaurant id")
	if !ok {
		return
	}
	if err := h.editor.ReorderVenues(c.Request.Context(), actor, id, ids); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- request helpers ---

func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func pathUUID(c *gin.Context, param, msg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusBadRequest, msg)
		return uuid.Nil, false
	}
	return id, true
}

func parseUUIDs(c *gin.Context, raw []string, msg string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			response.Error(c.Writer, http.StatusBadRequest, msg)
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// --- request DTOs ---

type categoryRequest struct {
	Slug      string      `json:"slug"`
	Title     string      `json:"title"`
	TitleI18n domain.I18n `json:"title_i18n"`
	Position  int         `json:"position"`
	// IsActive is a pointer so an omitted field means "active" rather than
	// "switch this rubric off" — a panel build that predates the flag must not
	// silently hide a rubric every time somebody fixes its title.
	IsActive *bool `json:"is_active"`
}

func (r categoryRequest) toInput() uc.CategoryInput {
	active := true
	if r.IsActive != nil {
		active = *r.IsActive
	}
	return uc.CategoryInput{
		Slug: r.Slug, Title: r.Title, TitleI18n: r.TitleI18n,
		Position: r.Position, IsActive: active,
	}
}

type collectionRequest struct {
	Slug            string      `json:"slug"`
	Title           string      `json:"title"`
	TitleI18n       domain.I18n `json:"title_i18n"`
	Subtitle        string      `json:"subtitle"`
	SubtitleI18n    domain.I18n `json:"subtitle_i18n"`
	Description     string      `json:"description"`
	DescriptionI18n domain.I18n `json:"description_i18n"`
	CoverImageURL   *string     `json:"cover_image_url"`
	City            *string     `json:"city"`
	Position        int         `json:"position"`
}

func (r collectionRequest) toInput() uc.CollectionInput {
	in := uc.CollectionInput{
		Slug: r.Slug, Title: r.Title, TitleI18n: r.TitleI18n,
		Subtitle: r.Subtitle, SubtitleI18n: r.SubtitleI18n,
		Description: r.Description, DescriptionI18n: r.DescriptionI18n,
		CoverImageURL: r.CoverImageURL, Position: r.Position,
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

type publishRequest struct {
	// PublishedAt nil means "now". A future value schedules the publication.
	PublishedAt *time.Time `json:"published_at"`
}

type setCategoriesRequest struct {
	CategoryIDs []string `json:"category_ids"`
}

type attachVenueRequest struct {
	// EventID / PromoID — необязательная подсветка блока: событие ИЛИ акция.
	EventID      string      `json:"event_id"`
	PromoID      string      `json:"promo_id"`
	RestaurantID string      `json:"restaurant_id"`
	Note         string      `json:"note"`
	NoteI18n     domain.I18n `json:"note_i18n"`
}

type venueNoteRequest struct {
	Note     string      `json:"note"`
	NoteI18n domain.I18n `json:"note_i18n"`
}

type reorderRequest struct {
	// RestaurantIDs is the intended FINAL order, complete.
	RestaurantIDs []string `json:"restaurant_ids"`
}

// --- response DTOs ---

// adminCategoryResponse carries the raw translation map alongside the base ru
// value: the cabinet edits translations, it does not render them.
type adminCategoryResponse struct {
	ID        string      `json:"id"`
	Slug      string      `json:"slug"`
	Title     string      `json:"title"`
	TitleI18n domain.I18n `json:"title_i18n,omitempty"`
	Position  int         `json:"position"`
	IsActive  bool        `json:"is_active"`
}

func newAdminCategoryResponse(c domain.GuideCategory) adminCategoryResponse {
	return adminCategoryResponse{
		ID: c.ID.String(), Slug: c.Slug, Title: c.Title, TitleI18n: c.TitleI18n,
		Position: c.Position, IsActive: c.IsActive,
	}
}

type adminCollectionResponse struct {
	ID              string      `json:"id"`
	Slug            string      `json:"slug"`
	Title           string      `json:"title"`
	TitleI18n       domain.I18n `json:"title_i18n,omitempty"`
	Subtitle        string      `json:"subtitle"`
	SubtitleI18n    domain.I18n `json:"subtitle_i18n,omitempty"`
	Description     string      `json:"description"`
	DescriptionI18n domain.I18n `json:"description_i18n,omitempty"`
	CoverImageURL   *string     `json:"cover_image_url"`
	City            *string     `json:"city"`
	Status          string      `json:"status"`
	PublishedAt     *time.Time  `json:"published_at"`
	Position        int         `json:"position"`
	// VenueCount is the GUEST-visible count — how many venues a guest could open
	// right now — so the cabinet shows the same number the app does. The full
	// membership, deactivated venues included, is in the detail's venues array.
	VenueCount    int       `json:"venue_count"`
	CategorySlugs []string  `json:"category_slugs"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func newAdminCollectionResponse(c domain.GuideCollection) adminCollectionResponse {
	out := adminCollectionResponse{
		ID: c.ID.String(), Slug: c.Slug,
		Title: c.Title, TitleI18n: c.TitleI18n,
		Subtitle: c.Subtitle, SubtitleI18n: c.SubtitleI18n,
		Description: c.Description, DescriptionI18n: c.DescriptionI18n,
		CoverImageURL: c.CoverImageURL,
		Status:        string(c.Status), PublishedAt: c.PublishedAt,
		Position: c.Position, VenueCount: c.VenueCount,
		CategorySlugs: c.CategorySlugs, UpdatedAt: c.UpdatedAt,
	}
	if out.CategorySlugs == nil {
		out.CategorySlugs = []string{}
	}
	if c.City != nil {
		v := string(*c.City)
		out.City = &v
	}
	return out
}

type adminCollectionDetailResponse struct {
	adminCollectionResponse
	Venues     []adminVenueResponse    `json:"venues"`
	Categories []adminCategoryResponse `json:"categories"`
}

func newAdminCollectionDetail(d domain.GuideCollectionAdminDetail) adminCollectionDetailResponse {
	out := adminCollectionDetailResponse{
		adminCollectionResponse: newAdminCollectionResponse(d.GuideCollection),
		Venues:                  make([]adminVenueResponse, 0, len(d.Venues)),
		Categories:              make([]adminCategoryResponse, 0, len(d.Categories)),
	}
	for _, v := range d.Venues {
		out.Venues = append(out.Venues, adminVenueResponse{
			RestaurantID: v.RestaurantID.String(), Position: v.Position,
			Note: v.Note, NoteI18n: v.NoteI18n,
			Name: v.Name, Address: v.Address, CuisineType: v.CuisineType,
			City: string(v.City), PriceCategory: string(v.PriceCategory),
			PrimaryImageURL: v.PrimaryImageURL, IsActive: v.IsActive,
		})
	}
	for _, cat := range d.Categories {
		out.Categories = append(out.Categories, newAdminCategoryResponse(cat))
	}
	return out
}

// adminVenueResponse is a venue row in the cabinet. IsActive is the field the
// guest response does not have and the editor cannot work without: it is why a
// collection of eight venues shows a venue_count of seven.
type adminVenueResponse struct {
	RestaurantID    string      `json:"restaurant_id"`
	Position        int         `json:"position"`
	Note            string      `json:"note"`
	NoteI18n        domain.I18n `json:"note_i18n,omitempty"`
	Name            string      `json:"name"`
	Address         string      `json:"address"`
	CuisineType     string      `json:"cuisine_type"`
	City            string      `json:"city"`
	PriceCategory   string      `json:"price_category"`
	PrimaryImageURL *string     `json:"primary_image_url"`
	IsActive        bool        `json:"is_active"`
}

// setVenueHighlight ставит или снимает событие/акцию у блока подборки.
// @Summary     Set the event/promo a collection block highlights (superadmin)
// @Tags        admin
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Success     204 "no content"
// @Router      /api/v1/admin/gastroguide/collections/{id}/venues/{restaurantId}/highlight [put]
func (h *EditorHandler) setVenueHighlight(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id", "invalid collection id")
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "restaurantId", "invalid restaurant id")
	if !ok {
		return
	}
	var req venueHighlightRequest
	if !bindJSON(c, &req) {
		return
	}
	eventID, promoID, ok := parseHighlight(c, req.EventID, req.PromoID)
	if !ok {
		return
	}
	if err := h.editor.SetVenueHighlight(c.Request.Context(), actor, id, rid, eventID, promoID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// venueHighlightRequest — тело PUT .../highlight. Оба поля пустые = снять
// подсветку и оставить обычную карточку заведения.
type venueHighlightRequest struct {
	EventID string `json:"event_id"`
	PromoID string `json:"promo_id"`
}

// parseHighlight разбирает пару «событие или акция». Пустая строка означает
// «не задано»; заданы оба — это ошибка запроса, а не повод выбирать за
// редактора.
func parseHighlight(c *gin.Context, rawEvent, rawPromo string) (eventID, promoID *uuid.UUID, ok bool) {
	rawEvent = strings.TrimSpace(rawEvent)
	rawPromo = strings.TrimSpace(rawPromo)
	if rawEvent != "" && rawPromo != "" {
		response.Error(c.Writer, http.StatusUnprocessableEntity,
			"a block may highlight an event or a promo, not both")
		return nil, nil, false
	}
	if rawEvent != "" {
		parsed, err := uuid.Parse(rawEvent)
		if err != nil {
			response.Error(c.Writer, http.StatusBadRequest, "invalid event id")
			return nil, nil, false
		}
		eventID = &parsed
	}
	if rawPromo != "" {
		parsed, err := uuid.Parse(rawPromo)
		if err != nil {
			response.Error(c.Writer, http.StatusBadRequest, "invalid promo id")
			return nil, nil, false
		}
		promoID = &parsed
	}
	return eventID, promoID, true
}
