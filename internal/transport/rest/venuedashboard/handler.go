// Package venuedashboard exposes one restaurant's own dashboard.
//
// Mounted on the venue-scoped group (RequireRestaurantManager on ":id"), so the
// caller is already proven to manage the venue before anything here runs — the
// same gate as the bookings, menu and schedule screens.
package venuedashboard

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/venuedashboard"
)

// Handler serves the venue dashboard endpoints.
type Handler struct{ uc *uc.UseCase }

// NewHandler wires the usecase into a handler.
func NewHandler(u *uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterScoped mounts the endpoints on a group already gated by
// RequireRestaurantManager(..., "id").
func (h *Handler) RegisterScoped(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/dashboard/summary", h.summary)
	rg.GET("/restaurants/:id/dashboard/load", h.load)
}

func (h *Handler) summary(c *gin.Context) {
	rid, from, to, ok := h.args(c)
	if !ok {
		return
	}
	d, err := h.uc.Summary(c.Request.Context(), rid, from, to)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}

	byStatus := make([]gin.H, 0, len(d.ByStatus))
	for _, s := range d.ByStatus {
		byStatus = append(byStatus, gin.H{"status": string(s.Status), "count": s.Count})
	}
	reasons := make([]gin.H, 0, len(d.CancelReasons))
	for _, r := range d.CancelReasons {
		reasons = append(reasons, gin.H{"reason": r.Reason, "count": r.Count})
	}
	response.OK(c.Writer, gin.H{
		"from":              d.From.Format(time.RFC3339),
		"to":                d.To.Format(time.RFC3339),
		"total":             d.Total,
		"by_status":         byStatus,
		"cancelled_share":   d.CancelledShare,
		"avg_party_size":    d.AvgPartySize,
		"cancel_reasons":    reasons,
		"preorder_bookings": d.PreorderBookings,
		// Minor units, like every other amount in this API. The client formats;
		// nobody does arithmetic on a formatted string.
		"preorder_total_minor": d.PreorderTotalMinor,
	})
}

func (h *Handler) load(c *gin.Context) {
	rid, from, to, ok := h.args(c)
	if !ok {
		return
	}
	slots, err := h.uc.Load(c.Request.Context(), rid, from, to)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]gin.H, 0, len(slots))
	for _, s := range slots {
		out = append(out, gin.H{
			"weekday": s.Weekday, "hour": s.Hour,
			"bookings": s.Bookings, "guests": s.Guests,
		})
	}
	response.OK(c.Writer, gin.H{"slots": out})
}

// args reads the restaurant id and the optional period. A present-but-broken
// date is a 422 here; an absent one is left zero for the usecase to default.
func (h *Handler) args(c *gin.Context) (uuid.UUID, time.Time, time.Time, bool) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	from, ok := parseTimeQuery(c, "from")
	if !ok {
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	to, ok := parseTimeQuery(c, "to")
	if !ok {
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	return rid, from, to, true
}

// parseTimeQuery accepts RFC3339 or a bare YYYY-MM-DD, matching the platform
// dashboard so the two APIs do not take dates in different shapes.
func parseTimeQuery(c *gin.Context, key string) (time.Time, bool) {
	raw := c.Query(key)
	if raw == "" {
		return time.Time{}, true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, true
	}
	response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity,
		domain.CodeValidation, "invalid "+key+": expected RFC3339 or YYYY-MM-DD")
	return time.Time{}, false
}
