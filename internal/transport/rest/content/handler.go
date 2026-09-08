// Package content exposes the staff content-draft review-queue HTTP endpoints:
// list a restaurant's pending drafts, and approve/reject one. All routes mount
// on a group running middleware.Auth; the RBAC gate (PermRestaurantManage at
// the draft's own restaurant) is resolved inside usecase/content.
//
// There is deliberately NO external submission endpoint in this increment — a
// future AI parser submits drafts as an internal caller via the repository /
// usecase Submit method, not over HTTP. If one is ever added it must be gated
// behind auth (see the PR).
package content

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/content"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Handler serves the content-draft review endpoints.
type Handler struct{ facade uc.Facade }

// NewHandler builds the content-draft HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterStaffRoutes mounts the review-queue routes. Mount on a group running
// middleware.Auth; authorization is enforced in the usecase.
func (h *Handler) RegisterStaffRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/restaurants/:id/content-drafts", h.listPending)
	rg.POST("/admin/content-drafts/:draftId/approve", h.approve)
	rg.POST("/admin/content-drafts/:draftId/reject", h.reject)
}

func (h *Handler) listPending(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathUUID(c, "id", "invalid restaurant id")
	if !ok {
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListPending(c.Request.Context(), actor, rid, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]draftResponse, 0, len(items))
	for _, d := range items {
		out = append(out, draftToResponse(d))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) approve(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	did, ok := pathUUID(c, "draftId", "invalid draft id")
	if !ok {
		return
	}
	res, err := h.facade.Approve(c.Request.Context(), actor, did)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := gin.H{"draft": draftToResponse(*res.Draft)}
	if res.EventID != nil {
		out["event_id"] = res.EventID.String()
	}
	if res.PromoID != nil {
		out["promo_id"] = res.PromoID.String()
	}
	response.OK(c.Writer, out)
}

func (h *Handler) reject(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	did, ok := pathUUID(c, "draftId", "invalid draft id")
	if !ok {
		return
	}
	d, err := h.facade.Reject(c.Request.Context(), actor, did)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, draftToResponse(*d))
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

type draftResponse struct {
	ID                   string            `json:"id"`
	RestaurantID         string            `json:"restaurant_id"`
	Kind                 string            `json:"kind"`
	Source               string            `json:"source"`
	SourceRef            *string           `json:"source_ref,omitempty"`
	SourceURL            *string           `json:"source_url,omitempty"`
	RawPayload           json.RawMessage   `json:"raw_payload,omitempty"`
	SuggestedTitle       string            `json:"suggested_title"`
	SuggestedTitleI18n   map[string]string `json:"suggested_title_i18n,omitempty"`
	SuggestedDescription string            `json:"suggested_description"`
	SuggestedDescI18n    map[string]string `json:"suggested_description_i18n,omitempty"`
	SuggestedStartsAt    *string           `json:"suggested_starts_at,omitempty"`
	SuggestedEndsAt      *string           `json:"suggested_ends_at,omitempty"`
	SuggestedVenue       *string           `json:"suggested_venue,omitempty"`
	SuggestedTerms       *string           `json:"suggested_terms,omitempty"`
	Status               string            `json:"status"`
	ReviewedBy           *string           `json:"reviewed_by,omitempty"`
	ReviewedAt           *string           `json:"reviewed_at,omitempty"`
	CreatedEventID       *string           `json:"created_event_id,omitempty"`
	CreatedPromoID       *string           `json:"created_promo_id,omitempty"`
	CreatedAt            string            `json:"created_at"`
	UpdatedAt            string            `json:"updated_at"`
}

func draftToResponse(d domain.ContentDraft) draftResponse {
	return draftResponse{
		ID:                   d.ID.String(),
		RestaurantID:         d.RestaurantID.String(),
		Kind:                 string(d.Kind),
		Source:               string(d.Source),
		SourceRef:            d.SourceRef,
		SourceURL:            d.SourceURL,
		RawPayload:           json.RawMessage(d.RawPayload),
		SuggestedTitle:       d.SuggestedTitle,
		SuggestedTitleI18n:   d.SuggestedTitleI18n,
		SuggestedDescription: d.SuggestedDescription,
		SuggestedDescI18n:    d.SuggestedDescriptionI18n,
		SuggestedStartsAt:    formatTimePtr(d.SuggestedStartsAt),
		SuggestedEndsAt:      formatTimePtr(d.SuggestedEndsAt),
		SuggestedVenue:       d.SuggestedVenue,
		SuggestedTerms:       d.SuggestedTerms,
		Status:               string(d.Status),
		ReviewedBy:           uuidPtrString(d.ReviewedBy),
		ReviewedAt:           formatTimePtr(d.ReviewedAt),
		CreatedEventID:       uuidPtrString(d.CreatedEventID),
		CreatedPromoID:       uuidPtrString(d.CreatedPromoID),
		CreatedAt:            d.CreatedAt.Format(time.RFC3339),
		UpdatedAt:            d.UpdatedAt.Format(time.RFC3339),
	}
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

func uuidPtrString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}
