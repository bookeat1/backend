package freedompay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// Endpoint paths. The signature's script name is the LAST SEGMENT of each of
// these ("init_payment", "clearing", "cancel", "refund", "status_v2").
const (
	pathInitPayment = "/init_payment"
	pathClearing    = "/g2g/clearing"
	pathCancel      = "/g2g/cancel"
	pathRefund      = "/g2g/refund"
	pathStatus      = "/g2g/status_v2"
)

// Gateway is the Freedom Pay implementation of domain.PaymentGateway.
type Gateway struct {
	cfg  Config
	http *payment.Client
	log  *slog.Logger
	now  func() time.Time
}

var (
	_ domain.PaymentGateway    = (*Gateway)(nil)
	_ payment.MerchantIDFinder = (*Gateway)(nil)
)

// New builds the adapter. client may be nil, in which case a default one is
// created; tests pass an httptest-backed client.
func New(cfg Config, client *payment.Client, log *slog.Logger) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = payment.NewClient(nil, payment.DefaultConfig(), log)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Gateway{cfg: cfg, http: client, log: log, now: time.Now}, nil
}

// Name reports the provider code this adapter serves.
func (g *Gateway) Name() domain.PaymentProvider { return domain.ProviderFreedomPay }

// ---------------------------------------------------------------------------
// domain.PaymentGateway
// ---------------------------------------------------------------------------

// Authorize creates a two-stage payment and returns the hosted payment page.
//
// The guest has not paid yet at this point, so the status is `created`; the
// hold appears when the result callback arrives with pg_captured=0.
//
// IMPORTANT operational fact from the documentation: a two-stage payment that
// receives neither /g2g/clearing nor /g2g/cancel within 5 DAYS is cleared
// AUTOMATICALLY. Any booking SLA that lets a hold sit longer than that will
// silently charge the guest — PAYMENTS_HOLD_TTL must stay below five days for
// this provider.
func (g *Gateway) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("pg_order_id", req.PaymentID.String())
	params.Set("pg_amount", payment.FormatMinor(req.Amount.AmountMinor))
	params.Set("pg_currency", string(req.Amount.Currency))
	params.Set("pg_description", req.Description)
	// TODO(verify): pg_auto_clearing=0 is what selects the two-stage flow in
	// the PayBox lineage of this API, but docs.freedompay.kz does not render
	// the parameter table. Confirm on the sandbox that 0 holds the funds and
	// that /g2g/clearing is then required — if the flag is inverted here we
	// would be charging guests immediately, which is exactly the failure the
	// two-stage design exists to prevent.
	params.Set("pg_auto_clearing", "0")
	params.Set("pg_idempotency_key", req.IdempotencyKey)
	params.Set("pg_request_method", http.MethodPost)
	if req.CallbackURL != "" {
		params.Set("pg_result_url", req.CallbackURL)
	}
	if req.ReturnURL != "" {
		params.Set("pg_success_url", req.ReturnURL)
		params.Set("pg_failure_url", req.ReturnURL)
	}
	if req.CustomerPhone != "" {
		params.Set("pg_user_phone", req.CustomerPhone)
	}
	if req.CustomerEmail != "" {
		params.Set("pg_user_contact_email", req.CustomerEmail)
	}
	if g.cfg.TestingMode {
		params.Set("pg_testing_mode", "1")
	}
	if req.HoldTTL > 0 {
		// TODO(verify): pg_lifetime is documented as the lifetime of the
		// payment (the link), and it is unclear whether it also bounds the
		// hold. Sent as seconds; check on the sandbox what an expired
		// pg_lifetime does to an already authorised hold.
		params.Set("pg_lifetime", strconv.FormatInt(int64(req.HoldTTL/time.Second), 10))
	}
	// Merchant parameters are echoed back to pg_result_url. They must not start
	// with "pg_" — that namespace belongs to the gateway.
	for k, v := range req.Metadata {
		if strings.HasPrefix(strings.ToLower(k), "pg_") {
			return nil, fmt.Errorf("freedompay: metadata key %q uses the reserved pg_ prefix: %w", k, domain.ErrValidation)
		}
		params.Set(k, v)
	}
	params.Set("booking_id", req.BookingID.String())
	params.Set("purpose", string(req.Purpose))

	values, raw, err := g.call(ctx, "init_payment", pathInitPayment, params, true)
	if err != nil {
		return nil, err
	}

	providerID := strings.TrimSpace(values.Get("pg_payment_id"))
	if providerID == "" {
		return nil, fmt.Errorf("freedompay init_payment: no pg_payment_id: %w", payment.ErrProviderMalformed)
	}

	g.log.Info("freedompay payment initialised",
		slog.String("payment_id", req.PaymentID.String()),
		slog.String("provider_payment_id", providerID),
	)

	return &domain.GatewayPayment{
		ProviderPaymentID: providerID,
		Status:            domain.PaymentCreated,
		Amount:            req.Amount,
		PaymentURL:        strings.TrimSpace(values.Get("pg_redirect_url")),
		Raw:               raw,
	}, nil
}

// Capture clears a two-stage hold, optionally for less than the held amount.
func (g *Gateway) Capture(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("freedompay: capture amount must be positive: %w", domain.ErrValidation)
	}

	params := url.Values{}
	params.Set("pg_payment_id", providerPaymentID)
	// TODO(verify): whether pg_amount is optional for a full clearing and
	// whether a partial clearing is accepted at all on the KZ terminal.
	params.Set("pg_amount", payment.FormatMinor(amount.AmountMinor))

	// Clearing is retryable: /g2g/clearing on an already cleared payment is
	// expected to answer with an error envelope rather than clear twice.
	// TODO(verify): confirm that a repeated clearing is a no-op error and not
	// a second capture. Until then this call is retried only on transport
	// failures and 5xx, never on a 4xx answer.
	values, raw, err := g.call(ctx, "clearing", pathClearing, params, true)
	if err != nil {
		return nil, err
	}

	capturedMinor := amount.AmountMinor
	if v := strings.TrimSpace(values.Get("pg_clearing_amount")); v != "" {
		if m, err := payment.ParseMinor(v); err == nil && m > 0 {
			capturedMinor = m
		}
	}

	now := g.now().UTC()
	g.log.Info("freedompay payment cleared",
		slog.String("provider_payment_id", providerPaymentID),
		slog.String("amount", amount.String()),
	)
	return &domain.GatewayPayment{
		ProviderPaymentID: providerPaymentID,
		Status:            domain.PaymentCaptured,
		Amount:            domain.Money{AmountMinor: capturedMinor, Currency: amount.Currency},
		CapturedAt:        &now,
		Raw:               raw,
	}, nil
}

// Void releases a hold before the funds are taken (/g2g/cancel). It is only
// valid for a two-stage payment that has not been cleared — after clearing the
// operation is a Refund, which the usecase decides, not this adapter.
func (g *Gateway) Void(ctx context.Context, providerPaymentID string) error {
	if err := requireID(providerPaymentID); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("pg_payment_id", providerPaymentID)

	values, _, err := g.call(ctx, "cancel", pathCancel, params, true)
	if err != nil {
		return err
	}
	if st, known := mapOperationStatus(values.Get("pg_revoke_status")); known && st != domain.RefundSucceeded {
		return fmt.Errorf("freedompay cancel: revoke not accepted: %w", payment.ErrProviderRejected)
	}
	g.log.Info("freedompay hold released", slog.String("provider_payment_id", providerPaymentID))
	return nil
}

// Refund returns money from an already cleared payment. Partial refunds are
// supported; refunding an UNCLEARED two-stage payment is rejected by the
// gateway by design — that case is a Void.
func (g *Gateway) Refund(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayRefund, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("freedompay: refund amount must be positive: %w", domain.ErrValidation)
	}

	params := url.Values{}
	params.Set("pg_payment_id", providerPaymentID)
	// TODO(verify): the refund page lists pg_amount, pg_currency and
	// pg_payment_id, but not which of them is required for a PARTIAL refund —
	// the PayBox lineage used pg_refund_amount. Confirm on the sandbox with a
	// partial refund and, if pg_amount turns out to mean "the original amount",
	// switch to pg_refund_amount here.
	params.Set("pg_amount", payment.FormatMinor(amount.AmountMinor))
	params.Set("pg_currency", string(amount.Currency))

	values, raw, err := g.call(ctx, "refund", pathRefund, params, true)
	if err != nil {
		return nil, err
	}

	status, known := mapOperationStatus(values.Get("pg_refund_status"))
	if !known {
		g.log.Warn("freedompay unmapped refund status",
			slog.String("provider_payment_id", providerPaymentID),
			slog.String("status", values.Get("pg_refund_status")),
		)
	}

	g.log.Info("freedompay refund accepted",
		slog.String("provider_payment_id", providerPaymentID),
		slog.String("amount", amount.String()),
	)
	return &domain.GatewayRefund{
		ProviderRefundID: strings.TrimSpace(values.Get("pg_payment_refund_id")),
		Status:           status,
		Amount:           amount,
		Raw:              raw,
	}, nil
}

// Get reads FreedomPay's own view of a payment (/g2g/status_v2). This is the
// reconciliation path (spec §5).
func (g *Gateway) Get(ctx context.Context, providerPaymentID string) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("pg_payment_id", providerPaymentID)
	return g.status(ctx, params)
}

// FindByMerchantPaymentID reads a payment by OUR pg_order_id, for the window in
// which the acquirer-side id never reached us. See payment.MerchantIDFinder.
func (g *Gateway) FindByMerchantPaymentID(ctx context.Context, merchantPaymentID string) (*domain.GatewayPayment, error) {
	if strings.TrimSpace(merchantPaymentID) == "" {
		return nil, fmt.Errorf("freedompay: empty merchant payment id: %w", domain.ErrValidation)
	}
	params := url.Values{}
	params.Set("pg_order_id", merchantPaymentID)
	return g.status(ctx, params)
}

func (g *Gateway) status(ctx context.Context, params url.Values) (*domain.GatewayPayment, error) {
	values, raw, err := g.call(ctx, "status_v2", pathStatus, params, true)
	if err != nil {
		return nil, err
	}

	captured := isTrue(values.Get("pg_captured"))
	status, known := mapPaymentStatus(values.Get("pg_payment_status"), captured)
	if !known {
		g.log.Warn("freedompay unmapped payment status",
			slog.String("provider_payment_id", values.Get("pg_payment_id")),
			slog.String("status", values.Get("pg_payment_status")),
		)
	}
	// A refund recorded against the payment outranks the raw status word: the
	// ledger cares about money that came back, not about a gateway's wording.
	//
	// Verified on the sandbox 2026-07-22 (payment 1814868833, 40.00 of 100.00
	// refunded): status_v2 reports the refunded sum as a NEGATIVE number
	// ("pg_refund_amount=-40"), from the merchant's point of view. Take the
	// magnitude — a refund of -40 and of 40 mean the same money leaving us.
	if refunded, err := payment.ParseMinor(values.Get("pg_refund_amount")); err == nil && refunded != 0 {
		if refunded < 0 {
			refunded = -refunded
		}
		if total, err := payment.ParseMinor(values.Get("pg_amount")); err == nil && total > 0 {
			if refunded >= total {
				status = domain.PaymentRefunded
			} else {
				status = domain.PaymentPartiallyRefunded
			}
		}
	}

	amountMinor, _ := payment.ParseMinor(values.Get("pg_amount"))
	currency := domain.Currency(strings.ToUpper(strings.TrimSpace(values.Get("pg_currency"))))
	if !currency.Valid() {
		currency = domain.CurrencyKZT
	}

	out := &domain.GatewayPayment{
		ProviderPaymentID: strings.TrimSpace(values.Get("pg_payment_id")),
		Status:            status,
		Amount:            domain.Money{AmountMinor: amountMinor, Currency: currency},
		Raw:               raw,
	}
	if at := parsePaymentDate(values.Get("pg_payment_date")); at != nil {
		out.AuthorizedAt = at
		if captured {
			out.CapturedAt = at
		}
	}
	if status == domain.PaymentFailed {
		out.FailureCode = strings.TrimSpace(values.Get("pg_error_code"))
		out.FailureMessage = sanitise(values.Get("pg_error_description"))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// transport
// ---------------------------------------------------------------------------

// call signs and performs one Sync API request and returns the flattened,
// signature-verified response.
//
// Both directions are signed, and the answer's signature is checked here: an
// unsigned or wrongly signed answer is treated as malformed, because acting on
// "your refund succeeded" from an unauthenticated source is the same class of
// mistake as acting on an unsigned webhook.
func (g *Gateway) call(ctx context.Context, op, path string, params url.Values, idempotent bool) (url.Values, json.RawMessage, error) {
	signed := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			signed.Add(k, v)
		}
	}
	signed.Set("pg_merchant_id", g.cfg.MerchantID)
	signed.Set(saltParam, newSalt())
	signed.Set(sigParam, sign(scriptName(path), signed, g.cfg.SecretKey))

	header := http.Header{}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	header.Set("Accept", "application/xml")

	resp, err := g.http.Do(ctx, payment.Request{
		Provider:   domain.ProviderFreedomPay,
		Op:         op,
		Method:     http.MethodPost,
		URL:        g.cfg.BaseURL + path,
		Header:     header,
		Body:       []byte(signed.Encode()),
		Idempotent: idempotent,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("freedompay %s: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("freedompay %s: HTTP %d: %w", op, resp.StatusCode, payment.ErrProviderRejected)
	}

	values, err := decodeXML(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("freedompay %s: %w", op, payment.ErrProviderMalformed)
	}

	// pg_error_code 9998 means FreedomPay could not identify the merchant, so
	// it could not sign its answer either. That is the one legitimate unsigned
	// message — and it is still a hard failure for us.
	if values.Get("pg_error_code") == "9998" {
		return nil, nil, fmt.Errorf("freedompay %s: merchant not recognised: %w", op, payment.ErrProviderRejected)
	}
	if !verify(scriptName(path), values, g.cfg.SecretKey) {
		return nil, nil, fmt.Errorf("freedompay %s: response signature mismatch: %w", op, payment.ErrProviderMalformed)
	}

	raw := redactedPayload(values)
	if !strings.EqualFold(strings.TrimSpace(values.Get("pg_status")), statusOK) {
		return values, raw, fmt.Errorf("freedompay %s: %s (%s): %w",
			op,
			sanitise(values.Get("pg_error_description")),
			sanitise(values.Get("pg_error_code")),
			payment.ErrProviderRejected)
	}
	return values, raw, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func validateAuthorize(req domain.AuthorizeRequest) error {
	switch {
	case req.PaymentID == uuid.Nil:
		return fmt.Errorf("freedompay: authorize without a payment id: %w", domain.ErrValidation)
	case req.IdempotencyKey == "":
		return fmt.Errorf("freedompay: authorize without an idempotency key: %w", domain.ErrValidation)
	case req.Amount.AmountMinor <= 0:
		return fmt.Errorf("freedompay: authorize amount must be positive: %w", domain.ErrValidation)
	case !req.Amount.Currency.Valid():
		return fmt.Errorf("freedompay: unsupported currency %q: %w", req.Amount.Currency, domain.ErrValidation)
	}
	return nil
}

func requireID(providerPaymentID string) error {
	if strings.TrimSpace(providerPaymentID) == "" {
		return fmt.Errorf("freedompay: empty provider payment id: %w", domain.ErrValidation)
	}
	return nil
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// redactedPayload stores the gateway message for the audit trail with card data
// and the signature removed: pg_card_pan and friends have no business in
// payment_events, and pg_sig is a keyed digest we do not need to keep (spec §8).
func redactedPayload(values url.Values) json.RawMessage {
	const redacted = "[redacted]"
	sensitive := map[string]struct{}{
		"pg_card_pan": {}, "pg_card_exp": {}, "pg_card_owner": {}, "pg_card_brand": {},
		"pg_card_id": {}, "pg_card_token": {}, "pg_card_name": {}, "pg_card_hash": {},
		sigParam: {},
	}
	out := make(map[string]string, len(values))
	for k, vs := range values {
		if len(vs) == 0 {
			continue
		}
		if _, bad := sensitive[k]; bad {
			out[k] = redacted
			continue
		}
		out[k] = vs[0]
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// sanitise bounds a gateway message before it becomes part of an error.
func sanitise(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "rejected"
	}
	return s
}
