// Package stories exposes the restaurant story HTTP endpoints.
package stories

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
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

// RegisterAdminRoutes mounts the admin CRUD/reorder routes. Mount on a group
// running middleware.Auth; authorization (PermRestaurantManage at the story's
// own restaurant) is enforced inside the usecase, so no restaurant-manager gate
// is needed here — the same shape as the promos/events admin routes.
func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/restaurants/:id/stories", h.listAdmin)
	rg.POST("/admin/restaurants/:id/stories", h.create)
	rg.PUT("/admin/stories/:storyId", h.update)
	rg.DELETE("/admin/stories/:storyId", h.delete)
	rg.POST("/admin/restaurants/:id/stories/reorder", h.reorder)
}

func (h *Handler) list(c *gin.Context) {
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
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

func (h *Handler) listAdmin(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	items, err := h.facade.ListForAdmin(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]adminStoryResponse, 0, len(items))
	for i := range items {
		out = append(out, adminStoryToResponse(&items[i]))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) create(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	var req createStoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s, err := h.facade.CreateStory(c.Request.Context(), actor, uc.CreateInput{
		RestaurantID: rid,
		ImageURL:     req.ImageURL,
		Caption:      req.Caption,
		ActionURL:    req.ActionURL,
		SortOrder:    req.SortOrder,
		IsActive:     req.IsActive,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminStoryToResponse(s))
}

func (h *Handler) update(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	sid, ok := pathUUID(c, "storyId", "invalid story id")
	if !ok {
		return
	}
	var req updateStoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s, err := h.facade.UpdateStory(c.Request.Context(), actor, sid, uc.UpdateInput{
		ImageURL:  req.ImageURL,
		Caption:   req.Caption,
		ActionURL: req.ActionURL,
		SortOrder: req.SortOrder,
		IsActive:  req.IsActive,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminStoryToResponse(s))
}

func (h *Handler) delete(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	sid, ok := pathUUID(c, "storyId", "invalid story id")
	if !ok {
		return
	}
	if err := h.facade.DeleteStory(c.Request.Context(), actor, sid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

func (h *Handler) reorder(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	var req reorderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	orderedIDs := make([]uuid.UUID, 0, len(req.OrderedIDs))
	for _, raw := range req.OrderedIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "ordered_ids must be uuids")
			return
		}
		orderedIDs = append(orderedIDs, id)
	}
	if err := h.facade.ReorderStories(c.Request.Context(), actor, rid, orderedIDs); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "reordered"})
}

// --- helpers ---

func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

func pathUUID(c *gin.Context, param, msg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, msg)
		return uuid.Nil, false
	}
	return id, true
}

// --- request DTOs ---

// createStoryRequest is a new story. image_url is required and validated in the
// usecase; caption/sort_order/is_active are optional (nil ⇒ the usecase default:
// no caption, end-of-list, active).
type createStoryRequest struct {
	ImageURL string  `json:"image_url"`
	Caption  *string `json:"caption"`
	// action_url is where a TAP on the story sends the guest. Not to be
	// confused with image_url, which is where the picture itself lives.
	// Optional; when present it must be an http(s) link (validated in the
	// usecase by domain.ValidateExternalActionURL).
	ActionURL *string `json:"action_url"`
	SortOrder *int    `json:"sort_order"`
	IsActive  *bool   `json:"is_active"`
}

// updateStoryRequest is a partial edit: an omitted field is left unchanged. An
// empty/whitespace caption clears it (empty ⇒ null).
type updateStoryRequest struct {
	ImageURL *string `json:"image_url"`
	Caption  *string `json:"caption"`
	// Same three states as caption: omitted ⇒ unchanged, empty ⇒ the link is
	// removed, otherwise validated and stored.
	ActionURL *string `json:"action_url"`
	SortOrder *int    `json:"sort_order"`
	IsActive  *bool   `json:"is_active"`
}

// reorderRequest is the new left-to-right order of a restaurant's story ids.
type reorderRequest struct {
	OrderedIDs []string `json:"ordered_ids"`
}
