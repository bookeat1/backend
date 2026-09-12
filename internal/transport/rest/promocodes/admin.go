package promocodes

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/promocodes"
)

// AdminHandler serves the cabinet's promo-code CRUD. Mount it on the
// superadmin-only group: a code is platform content (it can point at a
// PLATFORM campaign that runs at every venue), so there is no per-restaurant
// scope to authorize against.
type AdminHandler struct{ editor uc.Editor }

// NewAdminHandler builds the cabinet promo-code handler.
func NewAdminHandler(e uc.Editor) *AdminHandler { return &AdminHandler{editor: e} }

// RegisterAdminRoutes mounts the CRUD on a group that already runs
// middleware.Auth + RequireRole(RoleAdmin).
func (h *AdminHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/promo-codes", h.list)
	rg.POST("/admin/promo-codes", h.create)
	rg.GET("/admin/promo-codes/:codeId", h.get)
	// PATCH, not PUT: a full replace is how the cabinet has already wiped
	// fields on promos and events, and pause/resume is exactly that call.
	rg.PATCH("/admin/promo-codes/:codeId", h.patch)
	rg.DELETE("/admin/promo-codes/:codeId", h.delete)
}

func (h *AdminHandler) list(c *gin.Context) {
	var filter domain.PromoCodeFilter
	if raw := c.Query("promotion_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(c.Writer, http.StatusBadRequest, "invalid promotion_id")
			return
		}
		filter.PromotionID = &id
	}
	for _, s := range c.QueryArray("status") {
		st := domain.PromoCodeStatus(s)
		if !st.Valid() {
			response.Error(c.Writer, http.StatusBadRequest, "invalid status "+s)
			return
		}
		filter.Statuses = append(filter.Statuses, st)
	}
	items, err := h.editor.List(c.Request.Context(), filter)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]adminPromoCodeResponse, 0, len(items))
	for _, it := range items {
		out = append(out, adminFromDomain(it))
	}
	response.OK(c.Writer, out)
}

func (h *AdminHandler) get(c *gin.Context) {
	id, ok := pathUUID(c, "codeId")
	if !ok {
		return
	}
	item, err := h.editor.Get(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminFromDomain(*item))
}

func (h *AdminHandler) create(c *gin.Context) {
	var req createPromoCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	in, err := req.toUsecase()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	var actorID uuid.UUID
	if au, ok := middleware.GetAuthUser(c.Request.Context()); ok {
		actorID = au.ID
	}
	item, err := h.editor.Create(c.Request.Context(), in, actorID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, adminFromDomain(*item))
}

func (h *AdminHandler) patch(c *gin.Context) {
	id, ok := pathUUID(c, "codeId")
	if !ok {
		return
	}
	var req patchPromoCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	in, err := req.toUsecase()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	item, err := h.editor.Patch(c.Request.Context(), id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, adminFromDomain(*item))
}

func (h *AdminHandler) delete(c *gin.Context) {
	id, ok := pathUUID(c, "codeId")
	if !ok {
		return
	}
	if err := h.editor.Delete(c.Request.Context(), id); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

func pathUUID(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid promo code id")
		return uuid.Nil, false
	}
	return id, true
}

// createPromoCodeRequest is the cabinet's create form.
type createPromoCodeRequest struct {
	Code           string  `json:"code"`
	PromotionID    string  `json:"promotion_id"`
	StartsAt       string  `json:"starts_at"`
	ExpiresAt      string  `json:"expires_at"`
	MaxUsesTotal   *int    `json:"max_uses_total"`
	MaxUsesPerUser *int    `json:"max_uses_per_user"`
	Status         *string `json:"status"`
}

func (r createPromoCodeRequest) toUsecase() (uc.CreatePromoCodeInput, error) {
	promoID, err := uuid.Parse(r.PromotionID)
	if err != nil {
		return uc.CreatePromoCodeInput{}, validation("promotion_id must be a uuid")
	}
	startsAt, err := parseTime(r.StartsAt)
	if err != nil {
		return uc.CreatePromoCodeInput{}, validation("starts_at must be RFC3339")
	}
	expiresAt, err := parseTime(r.ExpiresAt)
	if err != nil {
		return uc.CreatePromoCodeInput{}, validation("expires_at must be RFC3339")
	}
	in := uc.CreatePromoCodeInput{
		Code: r.Code, PromotionID: promoID, StartsAt: startsAt, ExpiresAt: expiresAt,
		MaxUsesTotal: r.MaxUsesTotal,
	}
	if r.MaxUsesPerUser != nil {
		in.MaxUsesPerUser = *r.MaxUsesPerUser
	}
	if r.Status != nil {
		st := domain.PromoCodeStatus(*r.Status)
		if !st.Valid() {
			return uc.CreatePromoCodeInput{}, validation("unknown status " + *r.Status)
		}
		in.Status = st
	}
	return in, nil
}

// patchPromoCodeRequest carries only what the cabinet wants to change. Absent
// fields are absent, not zero: max_uses_total uses a double pointer so that
// "no overall limit" (explicit null) is distinguishable from "don't touch it".
type patchPromoCodeRequest struct {
	Code           *string `json:"code"`
	StartsAt       *string `json:"starts_at"`
	ExpiresAt      *string `json:"expires_at"`
	MaxUsesTotal   **int   `json:"max_uses_total"`
	MaxUsesPerUser *int    `json:"max_uses_per_user"`
	Status         *string `json:"status"`
}

func (r patchPromoCodeRequest) toUsecase() (uc.PatchPromoCodeInput, error) {
	in := uc.PatchPromoCodeInput{
		Code: r.Code, MaxUsesTotal: r.MaxUsesTotal, MaxUsesPerUser: r.MaxUsesPerUser,
	}
	if r.StartsAt != nil {
		t, err := parseTime(*r.StartsAt)
		if err != nil {
			return uc.PatchPromoCodeInput{}, validation("starts_at must be RFC3339")
		}
		in.StartsAt = &t
	}
	if r.ExpiresAt != nil {
		t, err := parseTime(*r.ExpiresAt)
		if err != nil {
			return uc.PatchPromoCodeInput{}, validation("expires_at must be RFC3339")
		}
		in.ExpiresAt = &t
	}
	if r.Status != nil {
		st := domain.PromoCodeStatus(*r.Status)
		if !st.Valid() {
			return uc.PatchPromoCodeInput{}, validation("unknown status " + *r.Status)
		}
		in.Status = &st
	}
	return in, nil
}

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// validation turns a request-shape problem into the same 422 + machine code
// the usecase would produce, so a client branches on `code` and never on the
// message.
func validation(msg string) error {
	return domain.WithCode(domain.CodeValidation, fmt.Errorf("%s: %w", msg, domain.ErrValidation))
}

// adminPromoCodeResponse is the cabinet shape: the row plus the two derived
// numbers the screen needs — activations and the campaign's real state.
type adminPromoCodeResponse struct {
	ID             string  `json:"id"`
	Code           string  `json:"code"`
	PromotionID    string  `json:"promotion_id"`
	StartsAt       string  `json:"starts_at"`
	ExpiresAt      string  `json:"expires_at"`
	MaxUsesTotal   *int    `json:"max_uses_total"`
	MaxUsesPerUser int     `json:"max_uses_per_user"`
	Status         string  `json:"status"`
	Activations    int     `json:"activations"`
	PromoTitle     string  `json:"promo_title"`
	PromoStatus    string  `json:"promo_status"`
	PromoEndsAt    *string `json:"promo_ends_at"`
	PromoMissing   bool    `json:"promo_missing"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

func adminFromDomain(a uc.AdminPromoCode) adminPromoCodeResponse {
	c := a.Code
	out := adminPromoCodeResponse{
		ID: c.ID.String(), Code: c.Code, PromotionID: c.PromotionID.String(),
		StartsAt: rfc3339(c.StartsAt), ExpiresAt: rfc3339(c.ExpiresAt),
		MaxUsesTotal: c.MaxUsesTotal, MaxUsesPerUser: c.MaxUsesPerUser,
		Status: string(c.Status), Activations: a.Activations,
		PromoTitle: a.PromoTitle, PromoStatus: string(a.PromoStatus),
		PromoMissing: a.PromoMissing,
		CreatedAt:    rfc3339(c.CreatedAt), UpdatedAt: rfc3339(c.UpdatedAt),
	}
	if !a.PromoEndsAt.IsZero() {
		s := rfc3339(a.PromoEndsAt)
		out.PromoEndsAt = &s
	}
	return out
}

func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
