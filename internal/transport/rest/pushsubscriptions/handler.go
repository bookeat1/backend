// Package pushsubscriptions exposes the staff-facing web-push subscription
// endpoints. Routes must be registered on a group already protected by
// middleware.Auth — every operation acts on the caller's own user id only, and
// a registration is additionally authorized against the caller's staff
// membership of the target restaurant inside the usecase.
package pushsubscriptions

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/notifications"
)

// Handler serves POST/DELETE for browser push subscriptions.
type Handler struct{ subs *uc.SubscriptionUseCase }

// NewHandler builds the push-subscription handler.
func NewHandler(subs *uc.SubscriptionUseCase) *Handler { return &Handler{subs: subs} }

// RegisterRoutes mounts the endpoints on an authenticated group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/push/subscriptions")
	g.POST("", h.register)
	g.DELETE("", h.unregister)
}

type registerRequest struct {
	RestaurantID string `json:"restaurant_id" binding:"required"`
	Endpoint     string `json:"endpoint" binding:"required"`
	Keys         struct {
		P256dh string `json:"p256dh" binding:"required"`
		Auth   string `json:"auth" binding:"required"`
	} `json:"keys" binding:"required"`
}

// register stores (or refreshes) the caller's browser push subscription for a
// venue they are staff of.
// @Summary     Register my browser push subscription
// @Tags        push
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body registerRequest true "PushSubscription + target restaurant"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     403 {object} response.Envelope "not staff of this restaurant"
// @Failure     422 {object} response.Envelope "invalid body"
// @Router      /api/v1/push/subscriptions [post]
func (h *Handler) register(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rid, err := uuid.Parse(req.RestaurantID)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	err = h.subs.Register(c.Request.Context(), au.ID, au.Role == string(domain.RoleAdmin), uc.RegisterInput{
		RestaurantID: rid,
		Endpoint:     req.Endpoint,
		P256dh:       req.Keys.P256dh,
		Auth:         req.Keys.Auth,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "registered"})
}

type unregisterRequest struct {
	Endpoint string `json:"endpoint" binding:"required"`
}

// unregister removes the caller's own subscription by endpoint. Idempotent.
// @Summary     Unregister my browser push subscription
// @Tags        push
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body unregisterRequest true "The endpoint to remove"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid body"
// @Router      /api/v1/push/subscriptions [delete]
func (h *Handler) unregister(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req unregisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.subs.Unregister(c.Request.Context(), au.ID, req.Endpoint); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "unregistered"})
}
