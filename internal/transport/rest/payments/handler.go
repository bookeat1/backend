// Package payments exposes the payment HTTP endpoints: the guest checkout
// flow, the venue's capture/void/settle actions, and the two acquirer webhook
// routes.
//
// # Guest access without an account (spec §6)
//
// A guest may pay for a booking, and later check on that payment, without
// ever logging in — a payment link opened cold from an SMS/email. Unlike
// bookings (every one of its routes requires middleware.Auth), this is a
// genuinely new case: the guest-facing routes here run behind
// middleware.OptionalAuth instead, which attaches an AuthUser when a valid
// token IS present but never rejects a request that has none. Which of the
// two the caller turns out to be — their own account, venue staff, or a truly
// anonymous guest — is decided entirely inside usecase/payments
// (authorizeRead / authorizeCreate / authorizeSettle), never here: this
// package's only job is to build the right usecase.Actor{UserID, Role} (nil
// UserID for anonymous) and hand it over.
//
// # PAYMENTS_ENABLED=false (spec, task's open decision)
//
// Every route in this package is ALWAYS mounted, master switch or not.
// Reasoning:
//   - Capture/Void/Settle/Status act on a payment that may already exist
//     (money already held or captured) — refusing them at the router level
//     whenever the switch is off would strand that money exactly the way
//     disabling an acquirer must never strand a refund (spec §9.1,
//     ADR-005's reconciler runs unconditionally for the same reason).
//   - CreateForBooking already reports the disabled case honestly and
//     precisely: usecase/payments.resolveSettings resolves cfg.Enabled
//     together with the venue's own override, and CreateForBooking returns
//     a clear domain.ErrValidation ("payments are not enabled for this
//     restaurant") that response.HandleError turns into a 422 — a client
//     sees exactly why, per venue, instead of a blanket "feature off".
//
// A global, router-level "everything answers 503 when PAYMENTS_ENABLED=false"
// was considered and rejected: it cannot distinguish "never configured, no
// payment could possibly exist" from "toggled off after payments already
// happened", and the second case is exactly the one that must keep working.
package payments

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	paymentgw "backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
	"backend-core/internal/infrastructure/payment/tiptoppay"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/payments"
)

// idempotencyHeader is the retry token required on POST /bookings/{id}/payment
// and .../settle (same convention and header name as bookings).
const idempotencyHeader = "Idempotency-Key"

// maxBodyBytes caps a guest/staff JSON request body.
const maxBodyBytes = 64 << 10

// maxWebhookBodyBytes caps a raw acquirer callback body. A legitimate
// FreedomPay/TipTopPay notification is a flat form post with a couple dozen
// short fields — this is generous headroom, not a real limit on normal
// traffic, and it exists purely so the endpoint (public, unauthenticated by
// design — see the package doc) cannot be used to exhaust memory.
const maxWebhookBodyBytes = 64 << 10

// freedomPayCallbackPath is OUR route below (RegisterWebhooks). It is also
// the last path segment FreedomPay's callback signature is computed against
// (freedompay.Config.ResultScriptName defaults to this same value) — the two
// must never drift apart.
const freedomPayCallbackPath = "/webhooks/payments/freedompay"

// Handler serves the payment endpoints.
type Handler struct {
	create   uc.CreateUseCase
	capture  uc.CaptureUseCase
	void     uc.VoidUseCase
	refund   uc.RefundUseCase
	webhook  uc.WebhookUseCase
	status   uc.StatusUseCase
	gateways *paymentgw.Registry
	// publicBaseURL is this backend's own externally-reachable origin, used
	// only to build the FreedomPay CallbackURL server-side (see the package
	// doc on PaymentsConfig.PublicBaseURL — never taken from the client).
	publicBaseURL string
}

// NewHandler wires the payment usecases into a handler. gateways is needed
// only to build each provider's own wire-format acknowledgement for its
// webhook (a signed XML envelope for FreedomPay, {"code":0} for TipTopPay) —
// no domain.PaymentGateway method carries a generic "build me an ack",
// because that shape genuinely differs per provider (spec §7 does not
// mandate one), so this is the one place transport reaches past the usecase
// layer into infrastructure/payment directly, same as
// usecase/payments deliberately never does.
func NewHandler(
	create uc.CreateUseCase,
	capture uc.CaptureUseCase,
	void uc.VoidUseCase,
	refund uc.RefundUseCase,
	webhook uc.WebhookUseCase,
	status uc.StatusUseCase,
	gateways *paymentgw.Registry,
	publicBaseURL string,
) *Handler {
	return &Handler{
		create: create, capture: capture, void: void, refund: refund,
		webhook: webhook, status: status, gateways: gateways,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}
}

// RegisterGuestRoutes mounts the checkout + read + settle routes. Mount on a
// group running middleware.OptionalAuth (see the package doc) — NOT
// middleware.Auth, which would reject the anonymous guest checkout this
// package exists to support.
func (h *Handler) RegisterGuestRoutes(rg *gin.RouterGroup) {
	rg.POST("/bookings/:id/payment", h.createPayment)
	rg.GET("/bookings/:id/payment", h.statusForBooking)
	rg.GET("/payments/:id", h.statusByID)
	rg.POST("/bookings/:id/payment/settle", h.settlePayment)
}

// RegisterStaffRoutes mounts the venue-only actions. Mount on a group running
// middleware.Auth — capture/void are staff actions on money already held, and
// unlike Create/Settle no guest ever legitimately calls them. The usecase
// itself still checks actor.staff() and restaurant ownership
// (authorizeStaffForRestaurant); this middleware choice is defence in depth,
// not the only gate.
func (h *Handler) RegisterStaffRoutes(rg *gin.RouterGroup) {
	rg.POST("/bookings/:id/payment/capture", h.captureHold)
	rg.POST("/bookings/:id/payment/void", h.voidHold)
}

// RegisterWebhooks mounts the two acquirer callback routes. Public, no
// middleware at all — authenticity comes from the provider's own signature,
// verified inside WebhookUseCase, never from a bearer token (spec §7:
// "публичные, без авторизации по токену").
func (h *Handler) RegisterWebhooks(rg *gin.RouterGroup) {
	g := rg.Group("/webhooks/payments")
	g.POST("/freedompay", h.freedomPayWebhook)
	g.POST("/tiptoppay/:type", h.tipTopPayWebhook)
}

// createPayment starts (or replays) the payment for a booking.
// @Summary     Pay for a booking
// @Description Computes the amount server-side, places a hold with the resolved acquirer, and returns a payment link. Works for a logged-in guest, an anonymous guest checkout link, or venue staff creating a payment on a guest's behalf.
// @Tags        payments
// @Accept      json
// @Produce     json
// @Param       id   path string              true "Booking id"
// @Param       body body createPaymentRequest true "Where the guest lands after the hosted payment page"
// @Param       Idempotency-Key header string true "Client retry token"
// @Success     201 {object} response.Envelope{data=paymentResponse}
// @Failure     404 {object} response.Envelope "booking not found"
// @Failure     409 {object} response.Envelope "booking already has an active payment"
// @Failure     422 {object} response.Envelope "validation failed / payments not enabled for this restaurant"
// @Router      /api/v1/bookings/{id}/payment [post]
func (h *Handler) createPayment(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req createPaymentRequest
	if err := bindJSON(c, &req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	callbackURL, err := h.callbackURL()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	p, err := h.create.CreateForBooking(c.Request.Context(), actor, uc.CreateInput{
		BookingID: id, IdempotencyKey: c.GetHeader(idempotencyHeader),
		ReturnURL: req.ReturnURL, CallbackURL: callbackURL,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// A replay (same idempotency key) returns 201 with the original payload —
	// same convention as bookings.createMine: the payment did come into
	// existence as a result of this (logical) request.
	response.Created(c.Writer, paymentToResponse(p))
}

// statusForBooking reads the booking's current live payment.
// @Summary     Get a booking's current payment
// @Description Returns the booking's live (authorized or captured) payment. Same access rule as reading the payment directly.
// @Tags        payments
// @Produce     json
// @Param       id path string true "Booking id"
// @Success     200 {object} response.Envelope{data=paymentResponse}
// @Failure     404 {object} response.Envelope "not found"
// @Router      /api/v1/bookings/{id}/payment [get]
func (h *Handler) statusForBooking(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	p, err := h.status.GetForBooking(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, paymentToResponse(p))
}

// statusByID reads one payment by its own id.
// @Summary     Get a payment
// @Description The payment's own owner, venue staff/admin of its restaurant, or (for a guest checkout with no account) anyone holding the id may read it.
// @Tags        payments
// @Produce     json
// @Param       id path string true "Payment id"
// @Success     200 {object} response.Envelope{data=paymentResponse}
// @Failure     404 {object} response.Envelope "not found"
// @Router      /api/v1/payments/{id} [get]
func (h *Handler) statusByID(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	p, err := h.status.Get(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, paymentToResponse(p))
}

// settlePayment resolves a cancelled or a no-show booking's captured payment
// into its final split (spec §9.1). Reachable by the guest themselves (a
// timely cancellation) and by venue staff/admin (a venue-caused cancellation
// or a no-show) — SettleInput.Trigger decides which, and the usecase enforces
// who may use which trigger.
// @Summary     Settle a cancelled or no-show booking's payment
// @Description Refunds the guest, keeps the venue's share, or both, depending on the trigger and timing (spec §9.1).
// @Tags        payments
// @Accept      json
// @Produce     json
// @Param       id   path string        true "Booking id"
// @Param       body body settleRequest true "Trigger + optional reason"
// @Param       Idempotency-Key header string true "Client retry token"
// @Success     200 {object} response.Envelope{data=paymentResponse}
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     404 {object} response.Envelope "not found"
// @Failure     409 {object} response.Envelope "already settled"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/bookings/{id}/payment/settle [post]
func (h *Handler) settlePayment(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req settleRequest
	if err := bindJSON(c, &req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput(c.GetHeader(idempotencyHeader))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	p, err := h.refund.Settle(c.Request.Context(), actor, id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, paymentToResponse(p))
}

// captureHold takes the booking's held deposit/pre-order when the venue seats
// the guest.
// @Summary     Capture a booking's held payment
// @Description Venue staff only. Idempotent: calling it again on an already-captured payment is a no-op.
// @Tags        payments
// @Produce     json
// @Security    BearerAuth
// @Param       id path string true "Booking id"
// @Success     200 {object} response.Envelope{data=paymentResponse}
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     404 {object} response.Envelope "not found"
// @Failure     422 {object} response.Envelope "invalid status transition"
// @Router      /api/v1/bookings/{id}/payment/capture [post]
func (h *Handler) captureHold(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	p, err := h.capture.CaptureOnSeating(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, paymentToResponse(p))
}

// voidHold releases a booking's held deposit/pre-order without charging the
// guest.
// @Summary     Release a booking's held payment
// @Description Venue staff only. Idempotent: calling it again on an already-voided payment is a no-op.
// @Tags        payments
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       id   path string      true "Booking id"
// @Param       body body voidRequest false "Optional reason"
// @Success     200 {object} response.Envelope{data=paymentResponse}
// @Failure     403 {object} response.Envelope "forbidden"
// @Failure     404 {object} response.Envelope "not found"
// @Failure     422 {object} response.Envelope "invalid status transition"
// @Router      /api/v1/bookings/{id}/payment/void [post]
func (h *Handler) voidHold(c *gin.Context) {
	actor := actorFrom(c)
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req voidRequest
	if !bindOptionalJSON(c, &req) {
		return
	}
	p, err := h.void.VoidOnRejection(c.Request.Context(), actor, id, req.reasonOrDefault())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, paymentToResponse(p))
}

// freedomPayWebhook is FreedomPay's result_url callback.
// @Summary     FreedomPay payment callback
// @Description Public, unauthenticated — authenticity is the pg_sig signature, verified server-side. Answers a signed XML envelope, never JSON. A bad signature or a processing failure both answer pg_status=error with no further detail, which makes FreedomPay retry every 30 minutes for up to 2 hours (sandbox-confirmed); a successful apply answers pg_status=ok.
// @Tags        payments
// @Accept      x-www-form-urlencoded
// @Produce     xml
// @Success     200 {string} string "<response><pg_status>ok</pg_status>...</response>"
// @Router      /webhooks/payments/freedompay [post]
func (h *Handler) freedomPayWebhook(c *gin.Context) {
	raw, ok := readWebhookBody(c)
	if !ok {
		return
	}
	err := h.webhook.HandleWebhook(c.Request.Context(), domain.ProviderFreedomPay, raw, headerMap(c.Request.Header))
	c.Data(http.StatusOK, "application/xml; charset=utf-8", h.freedomPayAck(err))
}

// freedomPayAck builds the signed XML FreedomPay expects in reply. Never
// reveals why an attempt failed (spec: don't confirm receipt, don't explain a
// bad signature) — every branch below already durably recorded the raw event
// in payment_events (signature_valid true or false) before this is reached,
// so answering "error" costs nothing beyond a retry FreedomPay was always
// going to attempt anyway on anything but a clean "ok".
func (h *Handler) freedomPayAck(err error) []byte {
	gw, gerr := h.gateways.ForRefund(domain.ProviderFreedomPay)
	fp, ok := gw.(*freedompay.Gateway)
	if gerr != nil || !ok {
		// Defensive fallback only: this route is only ever called by
		// FreedomPay itself, which implies the adapter IS configured. An
		// unsigned envelope is the best available answer with no secret key
		// to sign one.
		return []byte(`<?xml version="1.0" encoding="utf-8"?><response><pg_status>error</pg_status></response>`)
	}
	if err == nil {
		return fp.AckOK("")
	}
	return fp.AckError("")
}

// tipTopPayWebhook is one of TipTopPay's per-type notification routes
// (check/pay/fail/confirm/refund/cancel — see
// tiptoppay.NotificationTypeHeader's doc comment).
// @Summary     TipTopPay payment notification
// @Description Public, unauthenticated — authenticity is the Content-HMAC/X-Content-HMAC signature, verified server-side. Always answers {"code":0}, the only value TipTopPay's docs describe as "registered".
// @Tags        payments
// @Accept      x-www-form-urlencoded
// @Produce     json
// @Param       type path string true "check | pay | fail | confirm | refund | cancel"
// @Success     200 {object} map[string]interface{} "{\"code\":0}"
// @Router      /webhooks/payments/tiptoppay/{type} [post]
func (h *Handler) tipTopPayWebhook(c *gin.Context) {
	raw, ok := readWebhookBody(c)
	if !ok {
		return
	}
	headers := headerMap(c.Request.Header)
	headers[tiptoppay.NotificationTypeHeader] = c.Param("type")
	// The outcome (nil error or not) is deliberately not consulted here: see
	// the package doc's PAYMENTS_ENABLED note is unrelated — this is about
	// TipTopPay's OWN documented contract, which names only {"code":0} as
	// "registered" and describes no failure code.
	//
	// TODO(verify): confirm on the TipTopPay sandbox whether any other JSON
	// body or HTTP status makes it retry a notification, and whether that
	// would ever be worth doing (e.g. a transient DB outage while applying a
	// signature-valid event). Until then, always acknowledging is the
	// documented-safe choice, not a guess: the raw event is durably recorded
	// in payment_events either way, signature_valid reflecting the true
	// outcome, so nothing is silently lost by acking anyway.
	_ = h.webhook.HandleWebhook(c.Request.Context(), domain.ProviderTipTopPay, raw, headers)
	c.Data(http.StatusOK, "application/json; charset=utf-8", tiptoppay.AckBody())
}

// callbackURL builds the one webhook URL this backend hands an acquirer at
// Authorize time. Always FreedomPay-shaped: TipTopPay ignores CallbackURL
// entirely (its own merchant-cabinet configuration decides notification
// routes — see tiptoppay.Gateway.Authorize's doc comment), so there is no
// second shape to build.
func (h *Handler) callbackURL() (string, error) {
	if h.publicBaseURL == "" {
		return "", fmt.Errorf(
			"%w: payments are not configured (PAYMENTS_PUBLIC_BASE_URL is unset, cannot build the acquirer webhook callback)",
			domain.ErrValidation)
	}
	return h.publicBaseURL + freedomPayCallbackPath, nil
}

// actorFrom builds the usecase Actor from whatever middleware attached — an
// authenticated principal (their own account or venue staff/admin) or, when
// none is present (OptionalAuth let the request through anonymously), the
// zero Actor: UserID nil, Role empty. Never rejects; see the package doc.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	id := au.ID
	return uc.Actor{UserID: &id, Role: domain.Role(au.Role)}
}

// pathID parses a uuid path parameter, writing 422 on failure.
func pathID(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// bindJSON decodes a required JSON body under a size cap.
func bindJSON(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	return c.ShouldBindJSON(dst)
}

// bindOptionalJSON decodes a body that may legitimately be absent (a void
// without a reason). An empty body is fine; a malformed one is not.
func bindOptionalJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "cannot read request body")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid JSON body")
		return false
	}
	return true
}

// readWebhookBody reads the whole callback body under maxWebhookBodyBytes,
// verbatim — the signature is computed over these exact bytes, so nothing may
// pre-parse or normalise them first.
func readWebhookBody(c *gin.Context) ([]byte, bool) {
	if c.Request.Body == nil {
		response.Error(c.Writer, http.StatusBadRequest, "empty body")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBodyBytes+1))
	if err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "cannot read request body")
		return nil, false
	}
	if len(body) > maxWebhookBodyBytes {
		response.Error(c.Writer, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	return body, true
}

// headerMap flattens net/http's map[string][]string into map[string]string
// (first value only — every header this package's adapters read is
// single-valued). Both freedompay.header and tiptoppay.header already fall
// back to a case-insensitive scan, so the exact casing used here does not
// matter.
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
