// Package restaurants exposes the restaurant catalog HTTP endpoints.
package restaurants

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/restaurants"
)

type Handler struct {
	facade   uc.Facade
	managers uc.ManagerUseCase
}

func NewHandler(f uc.Facade, m uc.ManagerUseCase) *Handler {
	return &Handler{facade: f, managers: m}
}

// RegisterPublic mounts the unauthenticated catalog routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants", h.list)
	rg.GET("/restaurants/:id", h.get)
	rg.GET("/restaurant-categories", h.categories)
	rg.POST("/partnership-requests", h.submitPartnership)
}

// RegisterAdminGlobal mounts admin-only routes that are not scoped to a single
// restaurant (creating a new restaurant). Mount on a RequireRole(admin) group.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.POST("/restaurants", h.create)
}

// RegisterRestaurantScoped mounts mutations on an existing restaurant. Mount on
// a RequireRestaurantManager(..., "id") group (admin or the restaurant's manager).
func (h *Handler) RegisterRestaurantScoped(rg *gin.RouterGroup) {
	rg.PATCH("/restaurants/:id", h.update)
	rg.DELETE("/restaurants/:id", h.deactivate)
	rg.GET("/restaurants/:id/managers", h.listManagers)
	rg.POST("/restaurants/:id/managers", h.assignManager)
	rg.DELETE("/restaurants/:id/managers/:managerID", h.removeManager)
}

func (h *Handler) list(c *gin.Context) {
	f := domain.RestaurantFilter{Search: c.Query("search")}
	if v := c.Query("city"); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	if v := c.Query("category"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.Category = &id
		}
	}
	if v := c.Query("is_popular"); v != "" {
		b := v == "true"
		f.IsPopular = &b
	}
	if v := c.Query("is_new"); v != "" {
		b := v == "true"
		f.IsNew = &b
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))

	items, total, err := h.facade.List(c.Request.Context(), f)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]restaurantResponse, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it))
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	agg, err := h.facade.Get(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) categories(c *gin.Context) {
	cats, err := h.facade.Categories(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]categoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) submitPartnership(c *gin.Context) {
	var req partnershipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SubmitPartnership(c.Request.Context(), req.toInput()); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, gin.H{"status": "received"})
}

func (h *Handler) create(c *gin.Context) {
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	agg, err := h.facade.Create(c.Request.Context(), req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	agg, err := h.facade.Update(c.Request.Context(), id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) deactivate(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	if err := h.facade.SetActive(c.Request.Context(), id, false); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deactivated"})
}

func (h *Handler) listManagers(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	ms, err := h.managers.List(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]managerResponse, 0, len(ms))
	for _, m := range ms {
		out = append(out, managerToResponse(m))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) assignManager(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req assignManagerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	uid, err := uuid.Parse(req.UserID)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid user_id")
		return
	}
	var createdBy *uuid.UUID
	if au, ok := middleware.GetAuthUser(c.Request.Context()); ok {
		createdBy = &au.ID
	}
	m, err := h.managers.Assign(c.Request.Context(), uc.AssignManagerInput{
		RestaurantID: rid, UserID: uid, CreatedBy: createdBy,
		WhatsappOptIn: req.WhatsappOptIn, WhatsappPhone: req.WhatsappPhone,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, managerToResponse(*m))
}

func (h *Handler) removeManager(c *gin.Context) {
	mid, err := uuid.Parse(c.Param("managerID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid manager id")
		return
	}
	if err := h.managers.Remove(c.Request.Context(), mid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "removed"})
}
