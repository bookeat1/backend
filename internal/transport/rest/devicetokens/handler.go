// Package devicetokens exposes the GUEST-facing mobile push-token endpoints.
// Routes must be registered on a group already protected by middleware.Auth —
// every operation acts on the caller's own user id, taken from the token and
// never from the body or the path, so there is no restaurant/RBAC gate here
// (same shape as /users/me, /favorites, /notification-preferences).
package devicetokens

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/notifications"
)

// Handler serves POST/DELETE for a guest's mobile push tokens.
type Handler struct{ tokens *uc.DeviceTokenUseCase }

// NewHandler builds the device-token handler.
func NewHandler(tokens *uc.DeviceTokenUseCase) *Handler { return &Handler{tokens: tokens} }

// RegisterRoutes mounts the endpoints on an authenticated group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/devices/push-tokens")
	g.POST("", h.register)
	g.DELETE("", h.unregister)
}

type registerRequest struct {
	Token    string `json:"token" binding:"required"`
	Platform string `json:"platform" binding:"required"`
}

// register stores (or re-points) the caller's device push token.
// @Summary     Register my device for push notifications
// @Tags        push
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body registerRequest true "Provider push token + platform"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid body"
// @Router      /api/v1/devices/push-tokens [post]
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
	t, err := h.tokens.Register(c.Request.Context(), au.ID, uc.RegisterDeviceInput{
		Token:    req.Token,
		Platform: domain.DevicePlatform(req.Platform),
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// The token itself is never echoed back: it is a device credential, and the
	// caller already has it. The row id is enough to correlate with support.
	response.OK(c.Writer, gin.H{"id": t.ID, "platform": string(t.Platform), "status": "registered"})
}

type unregisterRequest struct {
	Token string `json:"token" binding:"required"`
}

// unregister silences one of the caller's own devices. Idempotent.
// @Summary     Unregister my device from push notifications
// @Tags        push
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body unregisterRequest true "The token to silence"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid body"
// @Router      /api/v1/devices/push-tokens [delete]
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
	if err := h.tokens.Unregister(c.Request.Context(), au.ID, req.Token); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "unregistered"})
}
