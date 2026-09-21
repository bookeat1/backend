// Package pushcampaigns exposes the superadmin push-campaign HTTP endpoints
// (spec push-campaigns-manual-spec-2026-09-17.md §4). Mounted on the plain
// authenticated group, NOT the RequireRole(RoleAdmin) group: GET .../estimate
// and POST are RoleAdmin-only (enforced in the usecase, like every other
// facade in this codebase), but GET (list/get) additionally allows a
// restaurant's own staff to read their venue's campaign STATUS, which needs a
// per-restaurant permission check the usecase resolves — see
// usecase/pushcampaigns.Facade's own authorization doc comments.
package pushcampaigns

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/pushcampaigns"
)

type Handler struct{ facade uc.Facade }

func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterRoutes mounts the endpoints. Mount on an authenticated group
// (middleware.Auth); every operation re-checks its own authorization.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/admin/push-campaigns")
	g.GET("/estimate", h.estimate)
	g.POST("", h.create)
	g.GET("", h.list)
	g.GET("/:id", h.get)
}

func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

func parseKind(c *gin.Context) (domain.PushCampaignKind, bool) {
	kind := domain.PushCampaignKind(c.Query("kind"))
	if !kind.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "kind must be 'event' or 'promo'")
		return "", false
	}
	return kind, true
}

// estimate previews a campaign's reach without creating anything.
// @Summary     Estimate a push campaign's reach
// @Tags        push-campaigns
// @Produce     json
// @Security    BearerAuth
// @Param       kind        query string true "event|promo"
// @Param       subject_id  query string true "Event or promo id"
// @Success     200 {object} response.Envelope{data=estimateResponse}
// @Failure     403 {object} response.Envelope "not RoleAdmin"
// @Failure     404 {object} response.Envelope "subject not found"
// @Router      /api/v1/admin/push-campaigns/estimate [get]
func (h *Handler) estimate(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	kind, ok := parseKind(c)
	if !ok {
		return
	}
	subjectID, err := uuid.Parse(c.Query("subject_id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid subject_id")
		return
	}
	res, err := h.facade.Estimate(c.Request.Context(), actor, kind, subjectID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newEstimateResponse(*res))
}

// create queues a new campaign.
// @Summary     Send a push campaign
// @Tags        push-campaigns
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body createRequest true "kind, subject_id, force_quiet_hours?"
// @Success     201 {object} response.Envelope{data=createdResponse}
// @Failure     403 {object} response.Envelope "not RoleAdmin"
// @Failure     404 {object} response.Envelope "subject not found"
// @Failure     422 {object} response.Envelope "subject_not_published | subject_expired | venue_inactive | city_unresolved | quiet_hours"
// @Failure     409 {object} response.Envelope "campaign_in_progress"
// @Failure     503 {object} response.Envelope "push_channel_disabled"
// @Router      /api/v1/admin/push-campaigns [post]
func (h *Handler) create(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	campaign, err := h.facade.Create(c.Request.Context(), actor, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, newCreatedResponse(*campaign))
}

// list returns the latest campaign per subject id for a restaurant, or for
// the platform's own subjects.
// @Summary     List latest push campaigns
// @Tags        push-campaigns
// @Produce     json
// @Security    BearerAuth
// @Param       kind          query string true  "event|promo"
// @Param       restaurant_id query string false "Required unless platform=true"
// @Param       platform      query bool   false "Platform's own subjects (RoleAdmin only)"
// @Success     200 {object} response.Envelope{data=[]summaryResponse}
// @Failure     403 {object} response.Envelope "not authorized for this restaurant"
// @Router      /api/v1/admin/push-campaigns [get]
func (h *Handler) list(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	kind, ok := parseKind(c)
	if !ok {
		return
	}
	platform := c.Query("platform") == "true"
	var restaurantID *uuid.UUID
	if raw := c.Query("restaurant_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid restaurant_id")
			return
		}
		restaurantID = &id
	}
	list, err := h.facade.ListLatestBySubjects(c.Request.Context(), actor, kind, restaurantID, platform)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]summaryResponse, 0, len(list))
	for _, camp := range list {
		out = append(out, newSummaryResponse(camp))
	}
	response.OK(c.Writer, out)
}

// get returns one campaign plus its skip-reason breakdown.
// @Summary     Get one push campaign
// @Tags        push-campaigns
// @Produce     json
// @Security    BearerAuth
// @Param       id path string true "Campaign id"
// @Success     200 {object} response.Envelope{data=detailResponse}
// @Failure     403 {object} response.Envelope "not authorized for this restaurant"
// @Failure     404 {object} response.Envelope "not found"
// @Router      /api/v1/admin/push-campaigns/{id} [get]
func (h *Handler) get(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid campaign id")
		return
	}
	res, err := h.facade.Get(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newDetailResponse(*res))
}
