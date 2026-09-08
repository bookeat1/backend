// Package favorites exposes the current user's favorite-restaurants HTTP
// endpoints. Routes must be registered on a group already protected by
// middleware.Auth — every operation acts on the caller's own user id only.
package favorites

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	restaurantsrest "backend-core/internal/transport/rest/restaurants"
	uc "backend-core/internal/usecase/favorites"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/favorites")
	g.GET("", h.list)
	g.PUT("/:restaurantId", h.add)
	g.DELETE("/:restaurantId", h.remove)
}

// list returns the authenticated caller's bookmarked, still-active
// restaurants, serialized identically to the public catalog listing.
// @Summary     List my favorite restaurants
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/favorites [get]
func (h *Handler) list(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.facade.List(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, restaurantsrest.PublicListItem(it, lang))
	}
	response.OK(c.Writer, out)
}

// add bookmarks a restaurant for the authenticated caller. Idempotent:
// bookmarking an already-favorited restaurant returns 200, not a conflict.
// @Summary     Add a restaurant to my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       restaurantId path string true "Restaurant id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     404 {object} response.Envelope "restaurant not found"
// @Failure     422 {object} response.Envelope "invalid restaurant id"
// @Router      /api/v1/favorites/{restaurantId} [put]
func (h *Handler) add(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Param("restaurantId"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	if err := h.facade.Add(c.Request.Context(), au.ID, rid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "favorited"})
}

// remove un-bookmarks a restaurant for the authenticated caller. Idempotent:
// removing something not favorited (or never favorited) still returns 200.
// @Summary     Remove a restaurant from my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       restaurantId path string true "Restaurant id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid restaurant id"
// @Router      /api/v1/favorites/{restaurantId} [delete]
func (h *Handler) remove(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Param("restaurantId"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	if err := h.facade.Remove(c.Request.Context(), au.ID, rid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "unfavorited"})
}
