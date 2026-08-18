package events

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/eventrecurrence"
)

// RecurrenceHandler serves the admin endpoints for RECURRENCE RULES ("every
// Wednesday at 19:00"), the things that generate events. It lives in this
// package so it shares the events handler's actor/uuid/pagination helpers and
// the identical RBAC story: mount on a group running middleware.Auth, and the
// PermRestaurantManage gate is resolved inside usecase/eventrecurrence.
//
// There are no public routes here on purpose. A guest never sees a rule — they
// see the events it generated, through the endpoints that already exist.
type RecurrenceHandler struct{ facade uc.Facade }

// NewRecurrenceHandler builds the recurrence rules HTTP handler.
func NewRecurrenceHandler(f uc.Facade) *RecurrenceHandler { return &RecurrenceHandler{facade: f} }

// RegisterAdminRoutes mounts the rule CRUD. Mount on a group running
// middleware.Auth.
//
// There is no DELETE: a rule is deactivated, never destroyed. Its occurrences
// reference it, some of them already happened and carry sold tickets, and a
// rule that is gone can no longer explain where those events came from.
func (h *RecurrenceHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/restaurants/:id/event-recurrences", h.create)
	rg.GET("/admin/restaurants/:id/event-recurrences", h.list)
	rg.GET("/admin/event-recurrences/:recurrenceId", h.get)
	rg.PUT("/admin/event-recurrences/:recurrenceId", h.update)
	rg.POST("/admin/event-recurrences/:recurrenceId/deactivate", h.deactivate)
	rg.POST("/admin/event-recurrences/:recurrenceId/activate", h.activate)
}

func (h *RecurrenceHandler) create(c *gin.Context) {
	actor, ok := recurrenceActor(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	in, ok := bindRecurrence(c)
	if !ok {
		return
	}
	in.RestaurantID = rid
	rec, err := h.facade.Create(c.Request.Context(), actor, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, recurrenceResponse(*rec))
}

func (h *RecurrenceHandler) update(c *gin.Context) {
	actor, ok := recurrenceActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "recurrenceId", "invalid recurrence id")
	if !ok {
		return
	}
	in, ok := bindRecurrence(c)
	if !ok {
		return
	}
	rec, err := h.facade.Update(c.Request.Context(), actor, id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, recurrenceResponse(*rec))
}

func (h *RecurrenceHandler) get(c *gin.Context) {
	actor, ok := recurrenceActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "recurrenceId", "invalid recurrence id")
	if !ok {
		return
	}
	rec, err := h.facade.Get(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, recurrenceResponse(*rec))
}

func (h *RecurrenceHandler) list(c *gin.Context) {
	actor, ok := recurrenceActor(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.List(c.Request.Context(), actor, rid, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]recurrenceResponseBody, 0, len(items))
	for _, rec := range items {
		out = append(out, recurrenceResponse(rec))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *RecurrenceHandler) deactivate(c *gin.Context) { h.setActive(c, false) }
func (h *RecurrenceHandler) activate(c *gin.Context)   { h.setActive(c, true) }

func (h *RecurrenceHandler) setActive(c *gin.Context, active bool) {
	actor, ok := recurrenceActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "recurrenceId", "invalid recurrence id")
	if !ok {
		return
	}
	if err := h.facade.SetActive(c.Request.Context(), actor, id, active); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"is_active": active})
}

// recurrenceActor reuses the events handler's actor extraction and re-types it:
// the two usecases keep separate Actor types (they are separate packages), but
// the identity they carry is the same authenticated user.
func recurrenceActor(c *gin.Context) (uc.Actor, bool) {
	a, ok := actorFrom(c)
	if !ok {
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: a.UserID, Role: a.Role}, true
}

// --- DTOs ---

// recurrenceRequest is the cabinet's payload. start_time is "HH:MM" wall clock
// and starts_on/until_date are "YYYY-MM-DD" — deliberately NOT timestamps: a
// rule describes what the clock on the wall says, and accepting an instant here
// would invite a client to bake its own timezone into the series.
type recurrenceRequest struct {
	Title            string            `json:"title"`
	TitleI18n        map[string]string `json:"title_i18n"`
	Description      string            `json:"description"`
	DescriptionI18n  map[string]string `json:"description_i18n"`
	Venue            string            `json:"venue"`
	CoverImageURL    *string           `json:"cover_image_url"`
	Tags             []string          `json:"tags"`
	OccurrenceStatus string            `json:"occurrence_status"`
	Ticketed         bool              `json:"ticketed"`
	TicketPriceMinor *int64            `json:"ticket_price_minor"`
	Capacity         *int              `json:"capacity"`
	// Refund rules for the tickets of every generated occurrence. Absent means
	// the conservative platform default, same reading as the event payload.
	TicketsRefundable         *bool `json:"tickets_refundable"`
	TicketRefundCutoffMinutes *int  `json:"ticket_refund_cutoff_minutes"`

	Frequency string `json:"frequency"`
	// Weekdays in ISO form: 1 = Monday … 7 = Sunday. Required for "weekly".
	Weekdays []int `json:"weekdays"`
	// MonthDay 1..31, required for "monthly".
	MonthDay *int `json:"month_day"`
	// StartTime is local wall-clock "HH:MM".
	StartTime       string `json:"start_time"`
	DurationMinutes int    `json:"duration_minutes"`
	// Timezone OVERRIDES the venue's zone. Omit it (or send "") to follow the
	// venue — which is what nearly every rule should do.
	Timezone  string  `json:"timezone"`
	StartsOn  string  `json:"starts_on"`
	UntilDate *string `json:"until_date"`
	// IsActive defaults to TRUE when absent: a rule you just created and did not
	// say anything about is a rule you want running.
	IsActive *bool `json:"is_active"`
}

func bindRecurrence(c *gin.Context) (uc.Input, bool) {
	var req recurrenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return uc.Input{}, false
	}
	startMinutes, err := parseWallClock(req.StartTime)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return uc.Input{}, false
	}
	startsOn, err := domain.ParseCalendarDate(req.StartsOn)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "starts_on must be YYYY-MM-DD")
		return uc.Input{}, false
	}
	var until *domain.CalendarDate
	if req.UntilDate != nil && strings.TrimSpace(*req.UntilDate) != "" {
		d, err := domain.ParseCalendarDate(*req.UntilDate)
		if err != nil {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "until_date must be YYYY-MM-DD")
			return uc.Input{}, false
		}
		until = &d
	}
	weekdays := make([]domain.ISOWeekday, 0, len(req.Weekdays))
	for _, w := range req.Weekdays {
		weekdays = append(weekdays, domain.ISOWeekday(w))
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	policy := domain.DefaultTicketRefundPolicy
	if req.TicketsRefundable != nil {
		policy.Refundable = *req.TicketsRefundable
	}
	if req.TicketRefundCutoffMinutes != nil {
		policy.CutoffMinutes = *req.TicketRefundCutoffMinutes
	}
	return uc.Input{
		Title:            req.Title,
		TitleI18n:        domain.I18n(req.TitleI18n),
		Description:      req.Description,
		DescriptionI18n:  domain.I18n(req.DescriptionI18n),
		Venue:            req.Venue,
		CoverImageURL:    req.CoverImageURL,
		Tags:             req.Tags,
		OccurrenceStatus: domain.EventStatus(req.OccurrenceStatus),
		Ticketed:         req.Ticketed,
		TicketPriceMinor: req.TicketPriceMinor,
		Capacity:         req.Capacity,
		RefundPolicy:     policy,
		Frequency:        domain.RecurrenceFrequency(req.Frequency),
		Weekdays:         weekdays,
		MonthDay:         req.MonthDay,
		StartMinutes:     startMinutes,
		DurationMinutes:  req.DurationMinutes,
		Timezone:         req.Timezone,
		StartsOn:         startsOn,
		UntilDate:        until,
		IsActive:         active,
	}, true
}

// parseWallClock reads "HH:MM" into minutes since local midnight. It refuses
// anything else rather than guessing: "19" or "7pm" would each be a different
// event for somebody.
func parseWallClock(v string) (int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("start_time must be HH:MM")
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("start_time hour must be 00..23")
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("start_time minute must be 00..59")
	}
	return h*60 + m, nil
}

type recurrenceResponseBody struct {
	ID               string            `json:"id"`
	RestaurantID     string            `json:"restaurant_id"`
	Title            string            `json:"title"`
	TitleI18n        map[string]string `json:"title_i18n,omitempty"`
	Description      string            `json:"description"`
	DescriptionI18n  map[string]string `json:"description_i18n,omitempty"`
	Venue            string            `json:"venue,omitempty"`
	CoverImageURL    *string           `json:"cover_image_url,omitempty"`
	Tags             []string          `json:"tags"`
	OccurrenceStatus string            `json:"occurrence_status"`
	Ticketed         bool              `json:"ticketed"`
	TicketPriceMinor *int64            `json:"ticket_price_minor,omitempty"`
	Capacity         *int              `json:"capacity,omitempty"`

	TicketsRefundable         bool `json:"tickets_refundable"`
	TicketRefundCutoffMinutes int  `json:"ticket_refund_cutoff_minutes"`

	Frequency string `json:"frequency"`
	// Always an array (never null): the cabinet renders weekday checkboxes and
	// an absent field would read as "unknown".
	Weekdays        []int  `json:"weekdays"`
	MonthDay        *int   `json:"month_day,omitempty"`
	StartTime       string `json:"start_time"`
	DurationMinutes int    `json:"duration_minutes"`
	// Timezone is the rule's OVERRIDE, empty when the rule follows its venue.
	Timezone  string  `json:"timezone,omitempty"`
	StartsOn  string  `json:"starts_on"`
	UntilDate *string `json:"until_date,omitempty"`
	IsActive  bool    `json:"is_active"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

func recurrenceResponse(r domain.EventRecurrence) recurrenceResponseBody {
	weekdays := make([]int, 0, len(r.Weekdays))
	for _, w := range r.Weekdays {
		weekdays = append(weekdays, int(w))
	}
	var until *string
	if r.UntilDate != nil {
		s := r.UntilDate.String()
		until = &s
	}
	return recurrenceResponseBody{
		ID:               r.ID.String(),
		RestaurantID:     r.RestaurantID.String(),
		Title:            r.Title,
		TitleI18n:        r.TitleI18n,
		Description:      r.Description,
		DescriptionI18n:  r.DescriptionI18n,
		Venue:            r.Venue,
		CoverImageURL:    r.CoverImageURL,
		Tags:             tagsOrEmpty(r.Tags),
		OccurrenceStatus: string(r.OccurrenceStatus),
		Ticketed:         r.Ticketed,
		TicketPriceMinor: r.TicketPriceMinor,
		Capacity:         r.Capacity,

		TicketsRefundable:         r.TicketsRefundable,
		TicketRefundCutoffMinutes: r.TicketRefundCutoffMinutes,

		Frequency:       string(r.Frequency),
		Weekdays:        weekdays,
		MonthDay:        r.MonthDay,
		StartTime:       fmt.Sprintf("%02d:%02d", r.StartMinutes/60, r.StartMinutes%60),
		DurationMinutes: r.DurationMinutes,
		Timezone:        r.Timezone,
		StartsOn:        r.StartsOn.String(),
		UntilDate:       until,
		IsActive:        r.IsActive,
		CreatedAt:       r.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       r.UpdatedAt.Format(time.RFC3339),
	}
}
