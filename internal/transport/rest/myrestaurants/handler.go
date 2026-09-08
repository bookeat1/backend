// Package myrestaurants exposes GET /admin/my-restaurants: the list of
// restaurants the authenticated caller is a staff member of, with the caller's
// role at each. It lets the admin panel offer a post-login restaurant picker
// instead of asking staff to type a restaurant UUID.
//
// Routes must be registered on a group already protected by middleware.Auth.
// The endpoint is NOT scoped to a single restaurant (it answers "which
// restaurants am I staff of"), so it does NOT sit behind
// middleware.RequireRestaurantManager — the usecase returns only the caller's
// own memberships (a superadmin gets every venue; see usecase/restaurants).
package myrestaurants

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/restaurants"
)

// Handler serves the my-restaurants endpoint.
type Handler struct{ uc *uc.MyRestaurantsUseCase }

// NewHandler wires the usecase into a handler.
func NewHandler(u *uc.MyRestaurantsUseCase) *Handler { return &Handler{uc: u} }

// RegisterRoutes mounts GET /admin/my-restaurants. Mount on a group running
// middleware.Auth (no RequireRestaurantManager — this route carries no
// restaurant id).
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/my-restaurants", h.list)
}

// restaurantResponse is one entry of the picker: the restaurant id, its name
// localized to the request locale, and the caller's role there
// ("owner"/"manager"/"hostess", or "admin" for a superadmin).
type restaurantResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Role string    `json:"role"`
}

// list returns the restaurants the authenticated caller is staff of.
// @Summary     List restaurants I am staff of
// @Description Restaurants where the caller is owner/manager/hostess, with their
// @Description role at each. A superadmin gets every restaurant (role "admin").
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/admin/my-restaurants [get]
func (h *Handler) list(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	actor := uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
	items, err := h.uc.List(c.Request.Context(), actor)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]restaurantResponse, 0, len(items))
	for _, it := range items {
		out = append(out, restaurantResponse{
			ID:   it.RestaurantID,
			Name: it.NameI18n.Resolve(lang, it.Name),
			Role: it.Role,
		})
	}
	response.OK(c.Writer, gin.H{"restaurants": out})
}
