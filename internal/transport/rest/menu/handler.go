// Package menu exposes the menu HTTP endpoints.
package menu

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/reqlocale"
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
	rg.GET("/menu-items/featured", h.featured)
	rg.GET("/restaurants/:id/menu-highlights", h.highlights)
}

// RegisterScoped mounts per-restaurant menu mutations on a group already gated
// by RequireRestaurantManager(..., "id").
func (h *Handler) RegisterScoped(rg *gin.RouterGroup) {
	rg.POST("/restaurants/:id/menu-items", h.create)
	rg.PATCH("/restaurants/:id/menu-items/:itemId", h.update)
	rg.DELETE("/restaurants/:id/menu-items/:itemId", h.delete)
	rg.PATCH("/restaurants/:id/menu-items/:itemId/availability", h.setAvailability)
	rg.PATCH("/restaurants/:id/menu-items/:itemId/featured", h.setFeatured)
	// «Лучшие позиции» of the venue's own storefront. Same group, same gate as
	// every other menu mutation: whoever may edit the menu may decide which of
	// its dishes the venue shows off. The repository still filters by
	// restaurant_id, which is what turns a guessed item id into a 404.
	rg.PATCH("/restaurants/:id/menu-items/:itemId/top-pick", h.setTopPick)
	rg.GET("/restaurants/:id/menu-top-picks", h.listTopPicks)
	rg.PUT("/restaurants/:id/menu-highlights", h.replaceTopPicks)
}

// RegisterAdmin mounts admin-only menu-category mutations.
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.POST("/menu-categories", h.createCategory)
	rg.PATCH("/menu-categories/:id", h.updateCategory)
	rg.DELETE("/menu-categories/:id", h.deleteCategory)
}

// list serves ONE venue's menu.
//
// The dish set never depends on the requested language — only the texts do.
// ?lang= / Accept-Language go through the shared reqlocale, so an unknown or
// untranslated language falls back to Russian instead of emptying the menu (it
// used to select ROWS by menu_items.language: ?lang=kk answered `[]` because
// the imported rows were labelled 'kz').
func (h *Handler) list(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	items, err := h.facade.ListByRestaurant(c.Request.Context(), rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, itemsToResponse(items, reqlocale.Resolve(c)))
}

func (h *Handler) categories(c *gin.Context) {
	cats, err := h.facade.Categories(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]menuCategoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat, lang))
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
	response.Created(c.Writer, itemToResponse(m, ""))
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
	response.OK(c.Writer, itemToResponse(m, ""))
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

// featured serves the cross-venue "chef's picks" rail of the main screen.
// City is a required query param, same as GET /feed: a rail of Almaty dishes
// shown to a guest in Astana is worse than no rail, so a missing or unknown
// city is a 422 with its own code rather than a silent country-wide list.
func (h *Handler) featured(c *gin.Context) {
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	items, err := h.facade.ListFeatured(c.Request.Context(), domain.City(c.Query("city")), limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]featuredItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, featuredToResponse(it, lang))
	}
	response.OK(c.Writer, out)
}

// setFeatured marks or unmarks one dish as an editorial pick. It is mounted on
// the venue-scoped group, so the caller is already a manager of :id; the
// repository still filters by restaurant_id, which is what turns a guessed item
// id into a 404 instead of a promotion of somebody else's dish.
func (h *Handler) setFeatured(c *gin.Context) {
	rid, itemID, ok := parseScoped(c)
	if !ok {
		return
	}
	var req featuredRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SetFeatured(c.Request.Context(), rid, itemID, req.IsFeatured); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

// highlights serves ONE venue's «Лучшие позиции» rail: the dishes the venue
// marked itself, in its order, then — only to fill the rail — the derived
// dishes the rail used to consist of entirely. Public, like GET .../menu.
//
// @Summary     A venue's «Лучшие позиции» rail
// @Tags        menu
// @Produce     json
// @Param       id     path  string true  "Restaurant ID"
// @Param       lang   query string false "Menu language"
// @Param       limit  query int    false "Rail size (default 8, max 24)"
// @Success     200 {array} menuItemResponse
// @Router      /api/v1/restaurants/{id}/menu-highlights [get]
func (h *Handler) highlights(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	items, err := h.facade.ListHighlights(c.Request.Context(), rid, limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, itemsToResponse(items, reqlocale.Resolve(c)))
}

// listTopPicks is the panel's editor view of the rail: what the venue marked,
// in its order, INCLUDING dishes that are currently stopped. It is venue-scoped
// (not public) precisely because it shows rows the guest rail hides.
//
// @Summary     What the venue marked as «Лучшие позиции» (venue manager)
// @Tags        menu
// @Produce     json
// @Param       id path string true "Restaurant ID"
// @Success     200 {array} menuItemResponse
// @Router      /api/v1/restaurants/{id}/menu-top-picks [get]
func (h *Handler) listTopPicks(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	items, err := h.facade.ListTopPicks(c.Request.Context(), rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// No localization: this is the venue's own editor view, which edits the
	// base text and renders the *_i18n maps itself.
	response.OK(c.Writer, itemsToResponse(items, ""))
}

// setTopPick marks or unmarks one dish. Marking takes the lowest free slot; a
// full rail is a 422 with its own code (menu_top_picks_limit) so the panel can
// say "снимите одно из отмеченных" instead of "validation failed".
//
// @Summary     Mark/unmark a dish as a «Лучшая позиция» (venue manager)
// @Tags        menu
// @Accept      json
// @Produce     json
// @Param       id     path string true "Restaurant ID"
// @Param       itemId path string true "Menu item ID"
// @Router      /api/v1/restaurants/{id}/menu-items/{itemId}/top-pick [patch]
func (h *Handler) setTopPick(c *gin.Context) {
	rid, itemID, ok := parseScoped(c)
	if !ok {
		return
	}
	var req topPickRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SetTopPick(c.Request.Context(), rid, itemID, req.IsTopPick); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

// replaceTopPicks sets the whole rail in one atomic call — what a drag-and-drop
// editor needs. An empty list clears the rail and the venue falls back to the
// derived one.
//
// @Summary     Replace the venue's «Лучшие позиции» order (venue manager)
// @Tags        menu
// @Accept      json
// @Produce     json
// @Param       id path string true "Restaurant ID"
// @Router      /api/v1/restaurants/{id}/menu-highlights [put]
func (h *Handler) replaceTopPicks(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	var req topPicksOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ids, err := req.toUUIDs()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	if err := h.facade.ReplaceTopPicks(c.Request.Context(), rid, ids); err != nil {
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
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	cat, err := h.facade.CreateCategory(c.Request.Context(), in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// Admin write echo: base text, not a localization of it.
	response.Created(c.Writer, categoryToResponse(*cat, ""))
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
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	cat, err := h.facade.UpdateCategory(c.Request.Context(), id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, categoryToResponse(*cat, ""))
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
