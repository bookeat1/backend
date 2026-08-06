// Package homefeeds exposes the mobile Home ("Explore") screen HTTP endpoints:
// the cuisine picker, the promotions strip and the articles block. All routes
// are public reads, mounted like the restaurant catalog.
package homefeeds

import (
	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/homefeeds"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the unauthenticated Home feed routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/cuisines", h.cuisines)
	rg.GET("/promotions", h.promotions)
	rg.GET("/articles", h.articles)
}

func (h *Handler) cuisines(c *gin.Context) {
	items, err := h.facade.Cuisines(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]cuisineResponse, 0, len(items))
	for _, it := range items {
		out = append(out, cuisineToResponse(it))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) promotions(c *gin.Context) {
	items, err := h.facade.Promotions(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]promotionResponse, 0, len(items))
	for _, it := range items {
		out = append(out, promotionToResponse(it))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) articles(c *gin.Context) {
	items, err := h.facade.Articles(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]articleResponse, 0, len(items))
	for _, it := range items {
		out = append(out, articleToResponse(it))
	}
	response.OK(c.Writer, out)
}
