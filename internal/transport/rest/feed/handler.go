// Package feed exposes the merchandising-feed HTTP endpoints — the app's
// main-screen "Акции" rail plus the venue and platform sides of it.
//
// Three mount points, matching the three postures in usecase/feed:
//   - RegisterPublic on the OptionalAuth group: the rail is public, but a
//     signed-in guest gets their cuisine preferences folded into the ranking.
//     A missing/invalid token behaves exactly like no token, never a 401 —
//     same contract as the public catalog.
//   - RegisterVenueRoutes on the authenticated group: the RBAC gate
//     (PermRestaurantManage at the item's OWN restaurant) is resolved inside
//     the usecase, because the path carries an item id, not a restaurant id.
//   - RegisterPlatformRoutes on the RequireRole(RoleAdmin) group: moderation
//     and the paid-placement dial. The usecase re-checks the superadmin role.
//
// Route shape note: the platform queue lives at /admin/feed/queue and the
// per-item routes under /admin/feed/items/:kind/:itemId, so a static segment
// and a wildcard never become siblings in the router tree.
package feed

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/feed"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Handler serves the feed endpoints.
type Handler struct{ facade uc.Facade }

// NewHandler builds the feed HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the guest main-screen rail. Mount on a group running
// middleware.OptionalAuth.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/feed", h.main)
}

// RegisterVenueRoutes mounts the venue side. Mount on a group running
// middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterVenueRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/restaurants/:id/feed", h.listVenue)
	rg.GET("/admin/feed/items/:kind/:itemId", h.getState)
	rg.POST("/admin/feed/items/:kind/:itemId/submit", h.submit)
	rg.POST("/admin/feed/items/:kind/:itemId/withdraw", h.withdraw)
}

// RegisterPlatformRoutes mounts the superadmin side. Mount on the
// RequireRole(RoleAdmin) group.
func (h *Handler) RegisterPlatformRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/feed/queue", h.listQueue)
	rg.POST("/admin/feed/items/:kind/:itemId/review", h.review)
	rg.PUT("/admin/feed/items/:kind/:itemId/placement-weight", h.setWeight)
}

// --- guest ---

func (h *Handler) main(c *gin.Context) {
	city := domain.City(c.Query("city"))
	if !city.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "city is required and must be a known city")
		return
	}
	// OptionalAuth: an anonymous guest simply carries no user id, and the
	// preference signal scores a neutral 0 for every card.
	var userID *uuid.UUID
	if au, ok := middleware.GetAuthUser(c.Request.Context()); ok {
		id := au.ID
		userID = &id
	}
	page, perPage := pagination(c)
	res, err := h.facade.Main(c.Request.Context(), uc.MainInput{
		City: city, UserID: userID, Page: page, PerPage: perPage,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]cardResponse, 0, len(res.Items))
	for _, r := range res.Items {
		out = append(out, newCardResponse(r, lang))
	}
	response.OK(c.Writer, response.NewPage(out, res.Total, res.Page, res.PerPage))
}

// --- venue ---

func (h *Handler) listVenue(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListVenue(c.Request.Context(), actor, rid, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]stateResponse, 0, len(items))
	for _, st := range items {
		out = append(out, newStateResponse(st))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) getState(c *gin.Context) {
	actor, kind, itemID, ok := actorAndTarget(c)
	if !ok {
		return
	}
	st, err := h.facade.GetState(c.Request.Context(), actor, kind, itemID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newStateResponse(*st))
}

func (h *Handler) submit(c *gin.Context) {
	actor, kind, itemID, ok := actorAndTarget(c)
	if !ok {
		return
	}
	item, err := h.facade.Submit(c.Request.Context(), actor, kind, itemID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newStateResponse(uc.ItemState{
		Item: *item, Lifecycle: domain.FeedLifecycleOf(*item, time.Now()),
	}))
}

func (h *Handler) withdraw(c *gin.Context) {
	actor, kind, itemID, ok := actorAndTarget(c)
	if !ok {
		return
	}
	item, err := h.facade.Withdraw(c.Request.Context(), actor, kind, itemID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newStateResponse(uc.ItemState{
		Item: *item, Lifecycle: domain.FeedLifecycleOf(*item, time.Now()),
	}))
}

// --- platform ---

func (h *Handler) listQueue(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListReviewQueue(c.Request.Context(), actor, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]stateResponse, 0, len(items))
	for _, st := range items {
		out = append(out, newStateResponse(st))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) review(c *gin.Context) {
	actor, kind, itemID, ok := actorAndTarget(c)
	if !ok {
		return
	}
	var req reviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	st, err := h.facade.Review(c.Request.Context(), actor, kind, itemID, uc.ReviewInput{
		Approve:         req.Approve,
		RejectionReason: req.RejectionReason,
		PlacementWeight: req.PlacementWeight,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newStateResponse(*st))
}

func (h *Handler) setWeight(c *gin.Context) {
	actor, kind, itemID, ok := actorAndTarget(c)
	if !ok {
		return
	}
	var req placementWeightRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	st, err := h.facade.SetPlacementWeight(c.Request.Context(), actor, kind, itemID, req.PlacementWeight)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, newStateResponse(*st))
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

// actorAndTarget resolves the three things every item-scoped route needs.
func actorAndTarget(c *gin.Context) (uc.Actor, domain.FeedItemKind, uuid.UUID, bool) {
	actor, ok := actorFrom(c)
	if !ok {
		return uc.Actor{}, "", uuid.Nil, false
	}
	kind := domain.FeedItemKind(c.Param("kind"))
	if !kind.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "kind must be 'promo' or 'event'")
		return uc.Actor{}, "", uuid.Nil, false
	}
	itemID, ok := pathUUID(c, "itemId", "invalid item id")
	if !ok {
		return uc.Actor{}, "", uuid.Nil, false
	}
	return actor, kind, itemID, true
}

func pathUUID(c *gin.Context, param, msg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, msg)
		return uuid.Nil, false
	}
	return id, true
}

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

// --- DTOs ---

type reviewRequest struct {
	// Approve is the decision. false = reject, which requires a reason.
	Approve bool `json:"approve"`
	// RejectionReason is what the venue must fix. Required on a rejection.
	RejectionReason string `json:"rejection_reason"`
	// PlacementWeight optionally prices the placement in the same call.
	// Omitted (null) leaves the current weight untouched.
	PlacementWeight *int `json:"placement_weight"`
}

type placementWeightRequest struct {
	PlacementWeight int `json:"placement_weight"`
}

// cardResponse is one card of the guest rail. It carries the score breakdown
// on purpose: the ranking is a product promise ("we show you what is relevant"),
// and a promise that cannot be inspected cannot be checked.
type cardResponse struct {
	Kind           string  `json:"kind"`
	ID             string  `json:"id"`
	RestaurantID   string  `json:"restaurant_id"`
	RestaurantName string  `json:"restaurant_name"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	StartsAt       string  `json:"starts_at"`
	EndsAt         string  `json:"ends_at"`
	CoverImageURL  *string `json:"cover_image_url,omitempty"`
	Terms          string  `json:"terms,omitempty"`
	Score          int     `json:"score"`
	// ScoreReasons is the full, ordered breakdown that produced Score.
	ScoreReasons []scoreReasonResponse `json:"score_reasons"`
}

type scoreReasonResponse struct {
	Code   string `json:"code"`
	Points int    `json:"points"`
	Detail string `json:"detail"`
}

func newCardResponse(r domain.RankedFeedItem, lang string) cardResponse {
	reasons := make([]scoreReasonResponse, 0, len(r.Score.Reasons))
	for _, reason := range r.Score.Reasons {
		reasons = append(reasons, scoreReasonResponse{
			Code: string(reason.Code), Points: reason.Points, Detail: reason.Detail,
		})
	}
	return cardResponse{
		Kind:           string(r.Item.Kind),
		ID:             r.Item.ID.String(),
		RestaurantID:   r.Item.RestaurantID.String(),
		RestaurantName: r.Item.RestaurantNameI18n.Resolve(lang, r.Item.RestaurantName),
		Title:          r.Item.TitleI18n.Resolve(lang, r.Item.Title),
		Description:    r.Item.DescriptionI18n.Resolve(lang, r.Item.Description),
		StartsAt:       r.Item.StartsAt.Format(time.RFC3339),
		EndsAt:         r.Item.EndsAt.Format(time.RFC3339),
		CoverImageURL:  r.Item.CoverImageURL,
		Terms:          r.Item.Terms,
		Score:          r.Score.Total,
		ScoreReasons:   reasons,
	}
}

// stateResponse is the venue/platform view of one item: the content, the
// moderation record and the derived lifecycle the cabinet renders as a badge.
type stateResponse struct {
	Kind            string  `json:"kind"`
	ID              string  `json:"id"`
	RestaurantID    string  `json:"restaurant_id"`
	RestaurantName  string  `json:"restaurant_name"`
	Title           string  `json:"title"`
	StartsAt        string  `json:"starts_at"`
	EndsAt          string  `json:"ends_at"`
	ItemStatus      string  `json:"item_status"`
	FeedStatus      string  `json:"feed_status"`
	Lifecycle       string  `json:"lifecycle"`
	SubmittedAt     *string `json:"submitted_at,omitempty"`
	ReviewedAt      *string `json:"reviewed_at,omitempty"`
	ReviewedBy      *string `json:"reviewed_by,omitempty"`
	RejectionReason *string `json:"rejection_reason,omitempty"`
	PlacementWeight int     `json:"placement_weight"`
}

func newStateResponse(st uc.ItemState) stateResponse {
	out := stateResponse{
		Kind:            string(st.Item.Kind),
		ID:              st.Item.ID.String(),
		RestaurantID:    st.Item.RestaurantID.String(),
		RestaurantName:  st.Item.RestaurantName,
		Title:           st.Item.Title,
		StartsAt:        st.Item.StartsAt.Format(time.RFC3339),
		EndsAt:          st.Item.EndsAt.Format(time.RFC3339),
		ItemStatus:      st.Item.ItemStatus,
		FeedStatus:      string(st.Item.Placement.Status),
		Lifecycle:       string(st.Lifecycle),
		RejectionReason: st.Item.Placement.RejectionReason,
		PlacementWeight: st.Item.Placement.PlacementWeight,
	}
	if t := st.Item.Placement.SubmittedAt; t != nil {
		s := t.Format(time.RFC3339)
		out.SubmittedAt = &s
	}
	if t := st.Item.Placement.ReviewedAt; t != nil {
		s := t.Format(time.RFC3339)
		out.ReviewedAt = &s
	}
	if id := st.Item.Placement.ReviewedBy; id != nil {
		s := id.String()
		out.ReviewedBy = &s
	}
	return out
}
