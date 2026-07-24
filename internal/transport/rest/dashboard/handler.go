// Package dashboard exposes the superadmin platform dashboard (Ф1):
// read-only, platform-wide aggregate statistics for the GLOBAL SUPERADMIN
// only. Every route mounts under /api/v1/admin/dashboard on a group already
// gated by middleware.RequireRole(domain.RoleAdmin); the usecase re-checks the
// superadmin role as defense-in-depth. A restaurant owner/manager/hostess or a
// guest never reaches the data — the router returns 403 first, and even if it
// did not, the usecase would.
package dashboard

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/dashboard"
)

// Handler serves the superadmin dashboard endpoints.
type Handler struct{ uc *uc.UseCase }

// NewHandler wires the dashboard usecase into a handler.
func NewHandler(u *uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterRoutes mounts the dashboard endpoints. The provided group MUST already
// enforce the superadmin gate (middleware.RequireRole(domain.RoleAdmin)); these
// routes carry no restaurant scope — they are platform-wide.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/dashboard/overview", h.overview)
	rg.GET("/admin/dashboard/bookings", h.bookings)
	rg.GET("/admin/dashboard/payments", h.payments)
	rg.GET("/admin/dashboard/top-restaurants", h.topRestaurants)
}

func (h *Handler) actor(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

// overview returns platform top-line counters.
// @Summary     Platform overview counters (superadmin)
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Router      /api/v1/admin/dashboard/overview [get]
func (h *Handler) overview(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	o, err := h.uc.Overview(c.Request.Context(), actor)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{
		"total_restaurants":     o.TotalRestaurants,
		"active_restaurants":    o.ActiveRestaurants,
		"total_users":           o.TotalUsers,
		"total_bookings":        o.TotalBookings,
		"bookings_last_7_days":  o.BookingsLast7Days,
		"bookings_last_30_days": o.BookingsLast30Days,
	})
}

// bookings returns booking counts by status over a period.
// @Summary     Bookings breakdown by status over a period (superadmin)
// @Tags        admin
// @Produce     json
// @Param       from query string false "period start (RFC3339 or YYYY-MM-DD)"
// @Param       to   query string false "period end (RFC3339 or YYYY-MM-DD)"
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     422 {object} response.Envelope "invalid period"
// @Router      /api/v1/admin/dashboard/bookings [get]
func (h *Handler) bookings(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	from, to, ok := parsePeriod(c)
	if !ok {
		return
	}
	b, err := h.uc.BookingsBreakdown(c.Request.Context(), actor, from, to)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	byStatus := make([]gin.H, 0, len(b.ByStatus))
	for _, s := range b.ByStatus {
		byStatus = append(byStatus, gin.H{"status": string(s.Status), "count": s.Count})
	}
	response.OK(c.Writer, gin.H{
		"from":      b.From.Format(time.RFC3339),
		"to":        b.To.Format(time.RFC3339),
		"total":     b.Total,
		"by_status": byStatus,
	})
}

// payments returns captured (GMV) and refunded money over a period.
// @Summary     Payments GMV and refunds over a period (superadmin)
// @Tags        admin
// @Produce     json
// @Param       from     query string false "period start (RFC3339 or YYYY-MM-DD)"
// @Param       to       query string false "period end (RFC3339 or YYYY-MM-DD)"
// @Param       currency query string false "ISO currency, default KZT"
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     422 {object} response.Envelope "invalid period"
// @Router      /api/v1/admin/dashboard/payments [get]
func (h *Handler) payments(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	from, to, ok := parsePeriod(c)
	if !ok {
		return
	}
	p, err := h.uc.PaymentsGMV(c.Request.Context(), actor, from, to, c.Query("currency"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// GMV = money PROCESSED through the platform (guest→restaurant), not BookEat
	// revenue; the platform take is ~zero under the subscription model. All
	// amounts are integer minor units of the reported currency.
	response.OK(c.Writer, gin.H{
		"from":     p.From.Format(time.RFC3339),
		"to":       p.To.Format(time.RFC3339),
		"currency": p.Currency,
		"captured": gin.H{"amount_minor": p.Captured.AmountMinor, "count": p.Captured.Count},
		"refunded": gin.H{"amount_minor": p.Refunded.AmountMinor, "count": p.Refunded.Count},
	})
}

// topRestaurants returns the top restaurants by bookings or by GMV.
// @Summary     Top restaurants by bookings or GMV over a period (superadmin)
// @Tags        admin
// @Produce     json
// @Param       from     query string false "period start (RFC3339 or YYYY-MM-DD)"
// @Param       to       query string false "period end (RFC3339 or YYYY-MM-DD)"
// @Param       by       query string false "ranking: bookings (default) or gmv"
// @Param       currency query string false "ISO currency for by=gmv, default KZT"
// @Param       limit    query int    false "max rows, default 10, max 50"
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     422 {object} response.Envelope "invalid period or ranking"
// @Router      /api/v1/admin/dashboard/top-restaurants [get]
func (h *Handler) topRestaurants(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	from, to, ok := parsePeriod(c)
	if !ok {
		return
	}
	by := c.Query("by")
	// A non-integer limit is silently defaulted (0 → usecase default), matching
	// the repo-wide ?page/?per_page convention.
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.uc.TopRestaurants(c.Request.Context(), actor, from, to, by, c.Query("currency"), limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		out = append(out, gin.H{
			"restaurant_id":  it.RestaurantID,
			"name":           it.NameI18n.Resolve(lang, it.Name),
			"bookings_count": it.BookingsCount,
			"gmv_minor":      it.GMVMinor,
		})
	}
	response.OK(c.Writer, gin.H{"restaurants": out})
}

// parsePeriod reads optional from/to query params. Absent → zero time (the
// usecase applies defaults). A present-but-unparseable value is a 422 (written
// here) and returns ok=false. Accepts RFC3339 or a bare YYYY-MM-DD date.
func parsePeriod(c *gin.Context) (from, to time.Time, ok bool) {
	from, ok = parseTimeQuery(c, "from")
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	to, ok = parseTimeQuery(c, "to")
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func parseTimeQuery(c *gin.Context, key string) (time.Time, bool) {
	v := c.Query(key)
	if v == "" {
		return time.Time{}, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, true
	}
	response.Error(c.Writer, http.StatusUnprocessableEntity, key+" must be an RFC3339 timestamp or YYYY-MM-DD date")
	return time.Time{}, false
}
