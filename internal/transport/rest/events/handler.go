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
	rg.GET("/events", h.listUpcoming)
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
	rg.PUT("/admin/events/:eventId/refund-policy", h.setRefundPolicy)
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

// listUpcoming serves the cross-venue guest listing (Explore screen):
// GET /events?city=&restaurant_id=&from=&to=&page=&per_page=&lang=.
// Unlike /restaurants/:id/events it is not tied to one venue; what a guest may
// see is decided in usecase/repository, never by a query parameter.
func (h *Handler) listUpcoming(c *gin.Context) {
	flt, ok := publicListFilter(c)
	if !ok {
		return
	}
	items, total, err := h.facade.ListPublicUpcoming(c.Request.Context(), flt)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]eventListItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, publicListItemResponse(it, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, flt.Page, flt.PerPage))
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
		City:             req.City,
		CoverImageURL:    req.CoverImageURL,
		Status:           domain.EventStatus(req.Status),
		Ticketed:         req.Ticketed,
		TicketPriceMinor: req.TicketPriceMinor,
		Capacity:         req.Capacity,
		Tags:             req.Tags,
		Images:           req.Images,
		RefundPolicy:     req.refundPolicy(),
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
	refundPolicy, ok := req.refundPolicyUpdate()
	if !ok {
		response.Error(c.Writer, http.StatusUnprocessableEntity,
			"tickets_refundable and ticket_refund_cutoff_minutes must be sent together; use PUT /admin/events/{id}/refund-policy to change only the refund rules")
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
		City:             req.City,
		CoverImageURL:    req.CoverImageURL,
		Status:           domain.EventStatus(req.Status),
		Ticketed:         req.Ticketed,
		TicketPriceMinor: req.TicketPriceMinor,
		Capacity:         req.Capacity,
		Tags:             req.Tags,
		Images:           req.Images,
		RefundPolicy:     refundPolicy,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminResponse(*e))
}

// setRefundPolicy lets an authorized venue role set THIS event's ticket-refund
// rules without re-sending the whole event. The RBAC gate
// (PermRestaurantManage at the event's own restaurant) lives in usecase/events,
// like every other admin event route.
func (h *Handler) setRefundPolicy(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	var req refundPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	e, err := h.facade.SetRefundPolicy(c.Request.Context(), actor, eid, domain.TicketRefundPolicy{
		Refundable:    req.TicketsRefundable,
		CutoffMinutes: req.CutoffMinutes,
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

// publicListFilter parses the cross-venue listing's query parameters. A
// malformed restaurant_id or date is answered with 422 rather than ignored:
// silently dropping restaurant_id would hand the caller the WHOLE platform's
// events under the name of one venue, and silently dropping a date bound would
// answer a different question than the one asked. An unknown `city` is left as
// it is (it simply matches nothing), same as the catalog listing.
func publicListFilter(c *gin.Context) (domain.PublicEventFilter, bool) {
	var f domain.PublicEventFilter
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
	from, ok := queryTime(c, "from")
	if !ok {
		return f, false
	}
	to, ok := queryTime(c, "to")
	if !ok {
		return f, false
	}
	f.From, f.To = from, to
	f.Page, f.PerPage = pagination(c)
	return f, true
}

// queryTime parses an optional RFC3339 query parameter, writing 422 on a
// malformed value. Same format the admin create/update payload uses.
func queryTime(c *gin.Context, name string) (*time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, name+" must be an RFC3339 timestamp")
		return nil, false
	}
	return &t, true
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
	Title           string            `json:"title"`
	TitleI18n       map[string]string `json:"title_i18n"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n"`
	StartsAt        string            `json:"starts_at"`
	EndsAt          string            `json:"ends_at"`
	Venue           string            `json:"venue"`
	CoverImageURL   *string           `json:"cover_image_url"`
	Status          string            `json:"status"`
	// City переопределяет город, в котором показывается событие. Пусто или
	// отсутствует — обычный случай: событие живёт в городе своего заведения, и
	// переезжает вместе с ним. Значение резолвится по справочнику городов
	// (принимается код «almaty», историческое написание, любой регистр);
	// неизвестный или скрытый город — 422, а не молча несуществующий фильтр.
	// Поле, как и всё в этой структуре, — полная замена: не прислали — сняли.
	City             *string `json:"city"`
	Ticketed         bool    `json:"ticketed"`
	TicketPriceMinor *int64  `json:"ticket_price_minor"`
	Capacity         *int    `json:"capacity"`
	// The «Афиша» chips ("Бранч", "Живая музыка", ...). The usecase trims blanks
	// and caps the count; absent/empty means the card draws no chips.
	Tags []string `json:"tags"`
	// Images — галерея события БЕЗ обложки, в порядке редактора. Отсутствующий
	// или пустой список очищает галерею (запись — полная замена, как и всё
	// остальное в этой структуре).
	Images []string `json:"images"`
	// The venue's own refund rules for this event. POINTERS on purpose: this is a
	// full-replace payload, and a cabinet build that predates the feature sends
	// the event without these fields. On create, absent means the conservative
	// default (not refundable). On update, absent means "leave the rules alone" —
	// otherwise editing a title from an older client would silently switch
	// refunds off for everyone who buys next.
	TicketsRefundable         *bool `json:"tickets_refundable"`
	TicketRefundCutoffMinutes *int  `json:"ticket_refund_cutoff_minutes"`
}

// refundPolicyRequest is the narrow "just the refund rules" admin payload.
type refundPolicyRequest struct {
	TicketsRefundable bool `json:"tickets_refundable"`
	CutoffMinutes     int  `json:"ticket_refund_cutoff_minutes"`
}

// refundPolicy is the CREATE reading: an absent field falls back to the ONE
// platform default (domain.DefaultTicketRefundPolicy), so an event created by a
// client that knows nothing about refunds gets the same rules as one created
// through the DB default — a review caught this drifting into a third,
// stricter default that contradicted the owner's decision.
func (r eventRequest) refundPolicy() domain.TicketRefundPolicy {
	p := domain.DefaultTicketRefundPolicy
	if r.TicketsRefundable != nil {
		p.Refundable = *r.TicketsRefundable
	}
	if r.TicketRefundCutoffMinutes != nil {
		p.CutoffMinutes = *r.TicketRefundCutoffMinutes
	}
	return p
}

// refundPolicyUpdate is the UPDATE reading: nil means "keep what the event
// already has". ok=false means the caller sent HALF a policy, which this
// endpoint refuses rather than guessing the other half — the narrow
// PUT .../refund-policy endpoint is the way to change one setting.
func (r eventRequest) refundPolicyUpdate() (policy *domain.TicketRefundPolicy, ok bool) {
	switch {
	case r.TicketsRefundable == nil && r.TicketRefundCutoffMinutes == nil:
		return nil, true
	case r.TicketsRefundable == nil || r.TicketRefundCutoffMinutes == nil:
		return nil, false
	}
	p := r.refundPolicy()
	return &p, true
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
	ID              string            `json:"id"`
	RestaurantID    string            `json:"restaurant_id"`
	Title           string            `json:"title"`
	TitleI18n       map[string]string `json:"title_i18n,omitempty"`
	Description     string            `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	StartsAt        string            `json:"starts_at"`
	EndsAt          string            `json:"ends_at"`
	Venue           string            `json:"venue,omitempty"`
	CoverImageURL   *string           `json:"cover_image_url,omitempty"`
	Status          string            `json:"status"`
	// City — переопределение города. Отсутствует, когда его нет: событие тогда
	// показывается в городе заведения-хозяина (см. restaurant.city рядом).
	City             *string `json:"city,omitempty"`
	Ticketed         bool    `json:"ticketed"`
	TicketPriceMinor *int64  `json:"ticket_price_minor,omitempty"`
	Capacity         *int    `json:"capacity,omitempty"`
	// The «Афиша» chips. Always present and serialized as [] when empty (never
	// null, never omitted): the app renders a chip row and an absent field would
	// read as "unknown".
	Tags []string `json:"tags"`
	// Images — дополнительные фотографии события, обложки среди них НЕТ. Всегда
	// массив (пустой, если фотографий нет): клиент рисует ленту и отсутствующее
	// поле читалось бы как «неизвестно».
	Images []string `json:"images"`
	// The refund rules a guest must be able to read BEFORE buying. Always
	// present (never omitempty): "false" is a rule too, and an absent field
	// would read as "unknown" in the app.
	TicketsRefundable         bool `json:"tickets_refundable"`
	TicketRefundCutoffMinutes int  `json:"ticket_refund_cutoff_minutes"`
	// RecurrenceID names the rule this occurrence was generated from, absent
	// for an ordinary one-off event. The cabinet uses it to tell the venue "this
	// date comes from a series" before they edit or delete it.
	RecurrenceID *string `json:"recurrence_id,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// cityOrNil renders the optional city override as a plain optional string. nil
// stays nil, which the response then omits entirely: "this event has no city of
// its own", not "its city is empty".
func cityOrNil(c *domain.City) *string {
	if c == nil {
		return nil
	}
	s := string(*c)
	return &s
}

// tagsOrEmpty guarantees the JSON tags field serializes as [] rather than null:
// a domain event read from the store is already non-nil, but events built
// in-memory (or by a fake) may carry a nil slice. Mirrors the feed's make([]T,0)
// approach to keeping empty lists honest arrays.
func tagsOrEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// adminResponse is the full staff-facing shape: base scalar + the raw i18n maps
// so the cabinet can edit translations.
func adminResponse(e domain.Event) eventResponse {
	var recurrenceID *string
	if e.RecurrenceID != nil {
		s := e.RecurrenceID.String()
		recurrenceID = &s
	}
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
		City:             cityOrNil(e.City),
		CoverImageURL:    e.CoverImageURL,
		Status:           string(e.Status),
		Ticketed:         e.Ticketed,
		TicketPriceMinor: e.TicketPriceMinor,
		Capacity:         e.Capacity,
		Tags:             tagsOrEmpty(e.Tags),
		Images:           tagsOrEmpty(e.Images),

		TicketsRefundable:         e.TicketsRefundable,
		TicketRefundCutoffMinutes: e.TicketRefundCutoffMinutes,
		RecurrenceID:              recurrenceID,
		CreatedAt:                 e.CreatedAt.Format(time.RFC3339),
		UpdatedAt:                 e.UpdatedAt.Format(time.RFC3339),
	}
}

// eventListItemResponse is the cross-venue listing's item: the same guest-facing
// event shape as the per-restaurant listing (embedded, so its fields stay inline
// and identical) plus the venue that hosts it, which the Explore card needs.
type eventListItemResponse struct {
	eventResponse
	Restaurant eventRestaurantResponse `json:"restaurant"`
}

// eventRestaurantResponse is the minimal venue identity on an Explore card.
type eventRestaurantResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	City string `json:"city"`
}

func publicListItemResponse(it domain.EventListItem, lang string) eventListItemResponse {
	return eventListItemResponse{
		eventResponse: publicResponse(it.Event, lang),
		Restaurant: eventRestaurantResponse{
			ID:   it.Restaurant.ID.String(),
			Name: it.Restaurant.NameI18n.Resolve(lang, it.Restaurant.Name),
			City: string(it.Restaurant.City),
		},
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
