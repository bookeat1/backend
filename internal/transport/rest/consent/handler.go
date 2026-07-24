// Package consent exposes the current user's data-processing consent and
// notification opt-out HTTP endpoints. Routes must be registered on a group
// already protected by middleware.Auth — every operation acts on the caller's
// own user id only (no cross-user access, no restaurant/RBAC scope).
package consent

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/consent"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/consents")
	g.GET("", h.currentState)
	g.POST("", h.record)

	p := rg.Group("/notification-preferences")
	p.GET("", h.getPreferences)
	p.PUT("", h.setPreferences)
}

// currentState returns the caller's effective consent state — the latest record
// per consent_type. History is preserved but not returned here.
// @Summary     Get my current consent state
// @Description Latest grant/revoke record per consent type for the authenticated user.
// @Tags        consent
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/consents [get]
func (h *Handler) currentState(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	recs, err := h.facade.CurrentState(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]consentResponse, 0, len(recs))
	for i := range recs {
		out = append(out, fromConsent(recs[i]))
	}
	response.OK(c.Writer, out)
}

// record appends one immutable grant/revoke record for the caller.
// @Summary     Record a consent decision
// @Description Appends one grant or revoke of a consent type at a policy version.
// @Description Append-only: prior records are never mutated, so the full history
// @Description is preserved as evidence.
// @Tags        consent
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body recordRequest true "Consent decision"
// @Success     201 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/consents [post]
func (h *Handler) record(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req recordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rec, err := h.facade.Record(c.Request.Context(), au.ID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, fromConsent(*rec))
}

// getPreferences returns the caller's notification opt-out (defaults when unset).
// @Summary     Get my notification preferences
// @Tags        consent
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/notification-preferences [get]
func (h *Handler) getPreferences(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	pref, err := h.facade.Preferences(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPreference(pref))
}

// setPreferences replaces the caller's notification opt-out.
// @Summary     Set my notification preferences
// @Tags        consent
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body preferenceRequest true "Notification preferences"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/notification-preferences [put]
func (h *Handler) setPreferences(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req preferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pref, err := h.facade.SetPreferences(c.Request.Context(), au.ID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPreference(pref))
}
