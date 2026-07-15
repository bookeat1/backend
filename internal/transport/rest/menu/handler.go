// Package menu exposes the menu HTTP endpoints.
package menu

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/menu"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts unauthenticated menu reads.
//
// The restaurant path param is named ":id" (NOT ":restaurantId") to match the
// Wave 1 restaurant routes. gin/httprouter forbids two different wildcard names
// at the same path position, so every route under /restaurants/:… MUST use ":id"
// or the router panics on startup.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/menu", h.list)
	rg.GET("/menu-categories", h.categories)
}

// RegisterScoped mounts per-restaurant menu mutations on a group already gated
// by RequireRestaurantManager(..., "id").
func (h *Handler) RegisterScoped(rg *gin.RouterGroup) {
	rg.POST("/restaurants/:id/menu-items", h.create)
	rg.PATCH("/restaurants/:id/menu-items/:itemId", h.update)
	rg.DELETE("/restaurants/:id/menu-items/:itemId", h.delete)
	rg.PATCH("/restaurants/:id/menu-items/:itemId/availability", h.setAvailability)
}

// RegisterAdmin mounts admin-only menu-category mutations.
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.POST("/menu-categories", h.createCategory)
	rg.PATCH("/menu-categories/:id", h.updateCategory)
	rg.DELETE("/menu-categories/:id", h.deleteCategory)
}

func (h *Handler) list(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	var lang *string
	if v := c.Query("lang"); v != "" {
		lang = &v
	}
	items, err := h.facade.ListByRestaurant(c.Request.Context(), rid, lang)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]menuItemResponse, 0, len(items))
	for i := range items {
		out = append(out, itemToResponse(&items[i]))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) categories(c *gin.Context) {
	cats, err := h.facade.Categories(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]menuCategoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) create(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	var req menuItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m, err := h.facade.Create(c.Request.Context(), rid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, itemToResponse(m))
}

func (h *Handler) update(c *gin.Context) {
	rid, itemID, ok := parseScoped(c)
	if !ok {
		return
	}
	var req menuItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m, err := h.facade.Update(c.Request.Context(), rid, itemID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, itemToResponse(m))
}

func (h *Handler) delete(c *gin.Context) {
	rid, itemID, ok := parseScoped(c)
	if !ok {
		return
	}
	if err := h.facade.Delete(c.Request.Context(), rid, itemID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

func (h *Handler) setAvailability(c *gin.Context) {
	rid, itemID, ok := parseScoped(c)
	if !ok {
		return
	}
	var req availabilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SetAvailable(c.Request.Context(), rid, itemID, req.IsAvailable); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

func (h *Handler) createCategory(c *gin.Context) {
	var req menuCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	cat, err := h.facade.CreateCategory(c.Request.Context(), req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, categoryToResponse(*cat))
}

func (h *Handler) updateCategory(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req menuCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	cat, err := h.facade.UpdateCategory(c.Request.Context(), id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, categoryToResponse(*cat))
}

func (h *Handler) deleteCategory(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	if err := h.facade.DeleteCategory(c.Request.Context(), id); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

// parseScoped extracts and validates the restaurant (:id) + :itemId path params.
func parseScoped(c *gin.Context) (uuid.UUID, uuid.UUID, bool) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return uuid.Nil, uuid.Nil, false
	}
	itemID, err := uuid.Parse(c.Param("itemId"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid item id")
		return uuid.Nil, uuid.Nil, false
	}
	return rid, itemID, true
}
