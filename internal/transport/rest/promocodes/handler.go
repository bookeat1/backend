// Package promocodes exposes the guest-facing promo-code endpoint: "would
// this code work for me, at this venue?", asked before the guest presses
// «Забронировать».
//
// The route is authenticated on purpose. A code's verdict is personal — the
// per-guest limit is part of it — and an anonymous precheck would both answer
// the wrong question and hand a scraper a free code oracle.
//
// Its answer is a SNAPSHOT and the client must treat it as one: the booking
// path re-decides the limit under the promo_codes row lock (ADR-047), so a
// code this endpoint calls good can still lose its last place a second later.
// That is why creating a booking with a code has its own refusals and its own
// "book without the code" fallback.
package promocodes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/promocodes"
)

// Handler serves the guest promo-code routes.
type Handler struct{ facade uc.Facade }

// NewHandler builds the promo-code HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterGuestRoutes mounts the precheck route. Mount it on a group running
// middleware.Auth: the verdict depends on WHO is asking.
func (h *Handler) RegisterGuestRoutes(rg *gin.RouterGroup) {
	rg.GET("/promo-codes/:code", h.precheck)
}

// precheck answers whether the code would be accepted for this guest at this
// venue, and returns the campaign text to show next to the field.
//
// @Summary  Check a promo code before booking
// @Tags     promo-codes
// @Produce  json
// @Param    code          path  string true  "Promo code as typed by the guest"
// @Param    restaurant_id query string true  "Restaurant the booking is for"
// @Success  200 {object} response.Envelope{data=promoCodeResponse}
// @Failure  401 {object} response.Envelope
// @Failure  404 {object} response.Envelope "promo_code_not_found"
// @Failure  422 {object} response.Envelope "promo_code_inactive / _expired / _not_started / _wrong_venue / _limit_reached / _already_used"
// @Security BearerAuth
// @Router   /promo-codes/{code} [get]
func (h *Handler) precheck(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	rid, err := uuid.Parse(c.Query("restaurant_id"))
	if err != nil {
		// The venue is required, not optional: a code may belong to one
		// restaurant's campaign, and answering "valid" without knowing where
		// the guest is booking would be a promise we cannot keep.
		response.Error(c.Writer, http.StatusBadRequest, "invalid restaurant_id")
		return
	}
	res, err := h.facade.Precheck(c.Request.Context(), c.Param("code"), rid, au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(*res, reqlocale.Resolve(c)))
}

// promoCodeResponse is what the guest's screen needs: the campaign behind the
// code and until when it is worth showing. There is deliberately no "uses
// left" field — it would be stale the moment it is serialized, and a client
// that renders a number will sooner or later render a wrong one.
type promoCodeResponse struct {
	// Code is the NORMALIZED code, so the client can echo back exactly what
	// will be stored rather than the guest's spelling.
	Code        string `json:"code"`
	PromotionID string `json:"promotion_id"`
	Title       string `json:"title"`
	Terms       string `json:"terms,omitempty"`
	// ValidUntil is the earlier of the code's and the campaign's end.
	ValidUntil string `json:"valid_until"`
}

func fromDomain(r domain.PromoCodeResolution, lang string) promoCodeResponse {
	return promoCodeResponse{
		Code:        r.Code,
		PromotionID: r.PromotionID.String(),
		Title:       r.TitleI18n.Resolve(lang, r.Title),
		Terms:       r.TermsI18n.Resolve(lang, r.Terms),
		ValidUntil:  r.ValidUntil.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
