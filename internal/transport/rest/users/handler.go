// Package users exposes the current-user profile HTTP endpoints. Routes must be
// registered on a group already protected by middleware.Auth.
package users

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/users"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/users")
	g.GET("/me", h.me)
	g.PATCH("/me", h.updateMe)
}

func (h *Handler) me(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := h.facade.Me(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u))
}

func (h *Handler) updateMe(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req updateMeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	u, err := h.facade.UpdateMe(c.Request.Context(), au.ID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u))
}
