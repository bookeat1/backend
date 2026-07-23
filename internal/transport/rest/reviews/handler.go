// Package reviews exposes the guest, public and staff review HTTP endpoints.
// The public listing/summary routes need no auth; the guest own-review routes
// mount on a group running middleware.Auth; the staff reply/moderate routes
// also mount on that authenticated group — the RBAC check (PermStaffManage at
// the review's own restaurant) is resolved inside usecase/reviews, not here,
// so transport only builds the Actor and forwards.
package reviews

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/reviews"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

type Handler struct{ facade uc.Facade }

// NewHandler builds the reviews HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the unauthenticated read routes: a restaurant's
// published reviews and its aggregate rating.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/reviews", h.listPublished)
	rg.GET("/restaurants/:id/reviews/summary", h.summary)
}

// RegisterGuestRoutes mounts the caller's own-review routes. Mount on a group
// running middleware.Auth.
func (h *Handler) RegisterGuestRoutes(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/reviews/me", h.getOwn)
	rg.PUT("/restaurants/:id/reviews/me", h.submit)
	rg.DELETE("/restaurants/:id/reviews/me", h.deleteOwn)
}

// RegisterStaffRoutes mounts the venue reply / moderation routes. Mount on a
// group running middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterStaffRoutes(rg *gin.RouterGroup) {
	rg.POST("/reviews/:reviewId/reply", h.reply)
	rg.PATCH("/reviews/:reviewId/status", h.moderate)
}

// staffActorFrom builds the usecase Actor from the authenticated user. ok is
// false (and a 401 already written) when no auth context is present.
func staffActorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

// listPublished returns a restaurant's published reviews, paginated.
// @Summary  List a restaurant's published reviews
// @Tags     reviews
// @Produce  json
// @Param    id path string true "Restaurant id"
// @Param    page query int false "1-based page"
// @Param    per_page query int false "page size (max 100)"
// @Success  200 {object} response.Envelope
// @Router   /api/v1/restaurants/{id}/reviews [get]
func (h *Handler) listPublished(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListPublished(c.Request.Context(), rid, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]reviewResponse, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// summary returns a restaurant's aggregate rating (average + count).
// @Summary  Restaurant aggregate rating
// @Tags     reviews
// @Produce  json
// @Param    id path string true "Restaurant id"
// @Success  200 {object} response.Envelope
// @Router   /api/v1/restaurants/{id}/reviews/summary [get]
func (h *Handler) summary(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	agg, err := h.facade.Rating(c.Request.Context(), rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg))
}

// getOwn returns the caller's own review for a restaurant.
// @Summary  Get my review for a restaurant
// @Tags     reviews
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Restaurant id"
// @Success  200 {object} response.Envelope
// @Failure  404 {object} response.Envelope "no review yet"
// @Router   /api/v1/restaurants/{id}/reviews/me [get]
func (h *Handler) getOwn(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	rv, err := h.facade.GetOwn(c.Request.Context(), au.ID, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, reviewToResponse(*rv))
}

// submit creates or edits the caller's own review. Idempotent per (user,
// restaurant): a second PUT overwrites the same review.
// @Summary  Create or edit my review
// @Tags     reviews
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id path string true "Restaurant id"
// @Param    body body submitRequest true "review"
// @Success  200 {object} response.Envelope
// @Failure  403 {object} response.Envelope "no completed booking at this restaurant"
// @Failure  404 {object} response.Envelope "restaurant not found"
// @Failure  422 {object} response.Envelope "invalid rating or body"
// @Router   /api/v1/restaurants/{id}/reviews/me [put]
func (h *Handler) submit(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rv, err := h.facade.Submit(c.Request.Context(), au.ID, uc.SubmitInput{
		RestaurantID: rid, Rating: req.Rating, Body: req.Body,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, reviewToResponse(*rv))
}

// deleteOwn removes the caller's own review (idempotent).
// @Summary  Delete my review
// @Tags     reviews
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Restaurant id"
// @Success  200 {object} response.Envelope
// @Router   /api/v1/restaurants/{id}/reviews/me [delete]
func (h *Handler) deleteOwn(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant id")
		return
	}
	if err := h.facade.DeleteOwn(c.Request.Context(), au.ID, rid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

// reply posts the venue's reply to a review.
// @Summary  Reply to a review (staff)
// @Tags     reviews
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    reviewId path string true "Review id"
// @Param    body body replyRequest true "reply"
// @Success  200 {object} response.Envelope
// @Failure  403 {object} response.Envelope "not staff of this restaurant"
// @Failure  404 {object} response.Envelope "review not found"
// @Router   /api/v1/reviews/{reviewId}/reply [post]
func (h *Handler) reply(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("reviewId"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid review id")
		return
	}
	var req replyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rv, err := h.facade.Reply(c.Request.Context(), actor, id, req.Reply)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, reviewToResponse(*rv))
}

// moderate hides or unhides a review.
// @Summary  Hide/unhide a review (staff)
// @Tags     reviews
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    reviewId path string true "Review id"
// @Param    body body moderateRequest true "status"
// @Success  200 {object} response.Envelope
// @Failure  403 {object} response.Envelope "not staff of this restaurant"
// @Failure  404 {object} response.Envelope "review not found"
// @Failure  422 {object} response.Envelope "invalid status"
// @Router   /api/v1/reviews/{reviewId}/status [patch]
func (h *Handler) moderate(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("reviewId"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid review id")
		return
	}
	var req moderateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	status := domain.ReviewStatus(req.Status)
	if !status.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "status must be one of: published, hidden")
		return
	}
	rv, err := h.facade.Moderate(c.Request.Context(), actor, id, status)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, reviewToResponse(*rv))
}

// pagination reads page/per_page query params, clamping to sane bounds.
func pagination(c *gin.Context) (page, perPage int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// --- request / response DTOs ---

type submitRequest struct {
	Rating int    `json:"rating"`
	Body   string `json:"body"`
}

type replyRequest struct {
	Reply string `json:"reply" binding:"required"`
}

type moderateRequest struct {
	Status string `json:"status" binding:"required"`
}

type reviewResponse struct {
	ID           string  `json:"id"`
	RestaurantID string  `json:"restaurant_id"`
	UserID       string  `json:"user_id"`
	AuthorName   string  `json:"author_name,omitempty"`
	Rating       int     `json:"rating"`
	Body         string  `json:"body"`
	Status       string  `json:"status"`
	OwnerReply   *string `json:"owner_reply,omitempty"`
	RepliedAt    *string `json:"replied_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type aggregateResponse struct {
	RestaurantID string  `json:"restaurant_id"`
	Average      float64 `json:"average"`
	Count        int     `json:"count"`
}

func reviewToResponse(rv domain.Review) reviewResponse {
	return reviewResponse{
		ID:           rv.ID.String(),
		RestaurantID: rv.RestaurantID.String(),
		UserID:       rv.UserID.String(),
		Rating:       rv.Rating,
		Body:         rv.Body,
		Status:       string(rv.Status),
		OwnerReply:   rv.OwnerReply,
		RepliedAt:    formatTimePtr(rv.RepliedAt),
		CreatedAt:    rv.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    rv.UpdatedAt.Format(time.RFC3339),
	}
}

func listItemToResponse(it domain.ReviewListItem) reviewResponse {
	r := reviewToResponse(it.Review)
	r.AuthorName = it.AuthorName
	return r
}

func aggregateToResponse(agg domain.RatingAggregate) aggregateResponse {
	return aggregateResponse{
		RestaurantID: agg.RestaurantID.String(),
		Average:      agg.Average,
		Count:        agg.Count,
	}
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
