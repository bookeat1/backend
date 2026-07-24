// Package preorder exposes the booking pre-order HTTP endpoints: read and
// replace a booking's pre-ordered menu items. Both routes are booking-scoped
// (/bookings/:id/preorder) and therefore cannot use RequireRestaurantManager
// (the path carries a booking id, not a restaurant id) — authorization is done
// inside usecase/preorder, which loads the booking and resolves the caller's
// relation to it (owner, venue staff, admin, or nobody). Same split as the
// booking-scoped booking routes.
package preorder

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/preorder"
)

// Handler serves the booking pre-order endpoints.
type Handler struct{ uc *uc.UseCase }

// NewHandler wires the pre-order usecase into a handler.
func NewHandler(u *uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterRoutes mounts the authenticated, booking-scoped pre-order routes.
// Mount on a group running middleware.Auth.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/bookings/:id/preorder", h.get)
	rg.PUT("/bookings/:id/preorder", h.replace)
}

func (h *Handler) get(c *gin.Context) {
	actor, bid, ok := actorAndBooking(c)
	if !ok {
		return
	}
	p, err := h.uc.Get(c.Request.Context(), actor, bid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponse(p))
}

func (h *Handler) replace(c *gin.Context) {
	actor, bid, ok := actorAndBooking(c)
	if !ok {
		return
	}
	var req replaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	lines, err := req.toLines()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	p, err := h.uc.Replace(c.Request.Context(), actor, bid, lines)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponse(p))
}

// actorAndBooking builds the pre-order Actor from the authenticated principal
// and parses the booking id, writing 401/422 on failure.
func actorAndBooking(c *gin.Context) (uc.Actor, uuid.UUID, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, uuid.Nil, false
	}
	bid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid booking id")
		return uc.Actor{}, uuid.Nil, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, bid, true
}
