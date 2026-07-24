// Package payouts exposes the restaurant-payout HTTP endpoints.
//
// Two RBAC tiers, both enforced INSIDE usecase/payouts (transport only builds
// the Actor and parses ids):
//
//   - Venue-scoped (owner/manager, restaurant.manage, tenant-scoped): set/read
//     the venue's payout destination and read its payout statement. Mounted on
//     the plain authed group, same as the admin panel's staff routes.
//   - Superadmin-only (money OUT): generate + send payouts. Mounted behind the
//     RequireRole(admin) group as defense-in-depth, and re-checked in the
//     usecase (authorizeSuperadmin).
package payouts

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/payouts"
)

// Handler serves the payout endpoints.
type Handler struct{ payouts *uc.UseCase }

// NewHandler wires the payout usecase into a handler.
func NewHandler(p *uc.UseCase) *Handler { return &Handler{payouts: p} }

// RegisterStaffRoutes mounts the venue-scoped routes. Mount on a group running
// middleware.Auth. Authorization (owner/manager, tenant-scoped) is in the
// usecase.
func (h *Handler) RegisterStaffRoutes(rg *gin.RouterGroup) {
	rg.PUT("/admin/restaurants/:id/payout-destination", h.setDestination)
	rg.GET("/admin/restaurants/:id/payout-destination", h.getDestination)
	rg.GET("/admin/restaurants/:id/payouts", h.listPayouts)
}

// RegisterSuperadminRoutes mounts the money-OUT routes. Mount on a group running
// middleware.Auth + RequireRole(admin). The usecase re-checks superadmin.
func (h *Handler) RegisterSuperadminRoutes(rg *gin.RouterGroup) {
	// Generate + send the full unpaid balance for one restaurant (manual
	// trigger; an automatic schedule would call the same usecase from a worker).
	rg.POST("/admin/restaurants/:id/payouts/generate", h.generateAndSendForRestaurant)
	// Generate pending payouts for every restaurant with a positive balance.
	rg.POST("/admin/payouts/generate", h.generateAll)
	// Send one already-generated pending payout.
	rg.POST("/admin/payouts/:payoutId/send", h.sendPayout)
}

// ---- destination -----------------------------------------------------------

func (h *Handler) setDestination(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req destinationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d, err := h.payouts.SetDestination(c.Request.Context(), actor, rid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, destinationToResponse(d))
}

func (h *Handler) getDestination(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	d, err := h.payouts.GetDestination(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, destinationToResponse(d))
}

func (h *Handler) listPayouts(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	list, err := h.payouts.ListForRestaurant(c.Request.Context(), actor, rid, 100)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, payoutsToResponse(list))
}

// ---- generate / send -------------------------------------------------------

func (h *Handler) generateAndSendForRestaurant(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	list, err := h.payouts.GenerateAndSendForRestaurant(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, payoutsToResponse(list))
}

func (h *Handler) generateAll(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	list, err := h.payouts.GenerateAll(c.Request.Context(), actor)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, payoutsToResponse(list))
}

func (h *Handler) sendPayout(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	pid, ok := pathUUID(c, "payoutId")
	if !ok {
		return
	}
	p, err := h.payouts.SendPayout(c.Request.Context(), actor, pid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, payoutToResponse(*p))
}

// ---- helpers ---------------------------------------------------------------

func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

func actorAndRID(c *gin.Context) (uc.Actor, uuid.UUID, bool) {
	actor, ok := actorFrom(c)
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	rid, ok := pathUUID(c, "id")
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	return actor, rid, true
}

func pathUUID(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}
