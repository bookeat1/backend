// Package roles exposes global role management to platform administrators.
//
// Mounted behind RequireRole(RoleAdmin). The usecase checks the role again:
// this is the endpoint that hands out the rights to every other endpoint, so it
// is the one place where a single forgotten gate is worth guarding twice.
package roles

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/roles"
)

// Handler serves the role endpoints.
type Handler struct{ uc *uc.UseCase }

// NewHandler wires the usecase in.
func NewHandler(u *uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterAdmin mounts the endpoints on a group already gated by
// RequireRole(domain.RoleAdmin).
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.GET("/admin/users", h.search)
	rg.PATCH("/admin/users/:id/role", h.setRole)
	rg.GET("/admin/users/:id/role-history", h.history)
}

type setRoleRequest struct {
	Role   string  `json:"role"`
	Reason *string `json:"reason"`
}

func (h *Handler) setRole(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid user id")
		return
	}
	var req setRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.uc.SetRole(c.Request.Context(), actor, id, domain.Role(req.Role), req.Reason); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

func (h *Handler) search(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	users, err := h.uc.Search(c.Request.Context(), actor, c.Query("q"), limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]gin.H, 0, len(users))
	for _, u := range users {
		out = append(out, gin.H{
			"id": u.ID, "email": u.Email, "phone": u.Phone,
			"full_name": u.FullName, "role": string(u.Role),
			"is_active": u.IsActive, "created_at": u.CreatedAt.Format(time.RFC3339),
		})
	}
	response.OK(c.Writer, gin.H{"users": out})
}

func (h *Handler) history(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid user id")
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	changes, err := h.uc.History(c.Request.Context(), actor, id, limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]gin.H, 0, len(changes))
	for _, ch := range changes {
		out = append(out, gin.H{
			"id": ch.ID, "actor_id": ch.ActorID,
			"from_role": string(ch.FromRole), "to_role": string(ch.ToRole),
			"reason": ch.Reason, "created_at": ch.CreatedAt.Format(time.RFC3339),
		})
	}
	response.OK(c.Writer, gin.H{"changes": out})
}

func (h *Handler) actor(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}
