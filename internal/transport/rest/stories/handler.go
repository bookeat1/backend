// Package stories exposes the restaurant story HTTP endpoints.
package stories

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/stories"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the unauthenticated story reads.
//
// The restaurant path param is named ":id" (NOT ":restaurantId") to match the
// Wave 1 restaurant routes and the menu handler: gin/httprouter forbids two
// different wildcard names at the same path position, so every route under
// /restaurants/:… MUST use ":id" or the router panics on startup.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/stories", h.list)
}

func (h *Handler) list(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	items, err := h.facade.List(c.Request.Context(), rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]storyResponse, 0, len(items))
	for i := range items {
		out = append(out, storyToResponse(&items[i]))
	}
	response.OK(c.Writer, out)
}
