package freedompay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// PayoutGateway is the Freedom Pay implementation of domain.PayoutGateway — the
// acquirer "выплаты" (payout) product. It uses the SAME merchant id and the
// SAME MD5 pg_sig signature scheme as the acquiring Gateway above; only the
// endpoints and the money direction differ.
//
// # What is documented vs. what is ASSUMED (money-moving; read before enabling)
//
// Confirmed from https://freedompay.kz/docs/gateway-api/payout (fetched
// 2026-07-24) — the endpoints, that they are pg_* form POSTs answered with
// signed XML, and that a saved-card (tokenized) payout is addressed by a card
// TOKEN, never a raw PAN:
//
//   - POST /g2g/reg2reg      — payout to a saved card, by (pg_user_id,
//     pg_card_token_to). This is the PCI-safe path this adapter uses.
//   - POST /g2g/payout_status2 — payout status by pg_order_id / pg_payment_id,
//     answering pg_payment_status ∈ {success, process, error}.
//
// NOT confirmed and marked TODO(verify) at each use — every one needs a real
// sandbox run once FreedomPay ENABLES the payout product for merchant 588079
// (their docs state payouts require manager approval + account configuration
// and have NO test mode, so this could not be verified here):
//
//   - the exact pg_payment_status value set and whether reg2reg answers it
//     synchronously or only asynchronously via pg_post_link + payout_status2;
//   - the format/necessity of pg_order_time_limit and whether pg_post_link is
//     mandatory;
//   - whether pg_currency is accepted/required (the merchant account currency
//     may be implicit);
//   - the exact (pg_user_id, pg_card_token_to) pairing produced by the card
//     tokenization step (which is itself not built here).
//
// Because this moves money, the adapter is wired but OFF by default: bootstrap
// only constructs it when FREEDOMPAY_PAYOUT_ENABLED=true, so nothing fires an
// unverified payout in production. It also NEVER retries the dispatch at the
// HTTP layer (Idempotent:false) — a resent payout could double-pay — and it
// NEVER reads an unknown status word as "paid".
type PayoutGateway struct {
	cfg  Config
	http *payment.Client
	log  *slog.Logger
	now  func() time.Time
}

var _ domain.PayoutGateway = (*PayoutGateway)(nil)

// Payout endpoint paths. The signature script name is the last path segment.
const (
	pathReg2Reg      = "/g2g/reg2reg"
	pathPayoutStatus = "/g2g/payout_status2"
)

// NewPayoutGateway builds the payout adapter. client may be nil.
func NewPayoutGateway(cfg Config, client *payment.Client, log *slog.Logger) (*PayoutGateway, error) {
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
	return &PayoutGateway{cfg: cfg, http: client, log: log, now: time.Now}, nil
}

// Name reports the provider code this adapter serves.
func (g *PayoutGateway) Name() domain.PaymentProvider { return domain.ProviderFreedomPay }

// Payout dispatches a single tokenized payout (/g2g/reg2reg). It is NEVER
// retried at the HTTP layer: a resent payout could pay twice.
func (g *PayoutGateway) Payout(ctx context.Context, req domain.PayoutRequest) (*domain.GatewayPayout, error) {
	if err := validatePayout(req); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("pg_order_id", req.PayoutID.String())
	params.Set("pg_amount", payment.FormatMinor(req.Amount.AmountMinor))
	params.Set("pg_user_id", req.DestinationCustomerRef)
	params.Set("pg_card_token_to", req.DestinationToken)
	params.Set("pg_description", payoutDescription(req.Description))
	if req.CallbackURL != "" {
		params.Set("pg_post_link", req.CallbackURL)
	}
	// TODO(verify): pg_order_time_limit format and whether it is required for
	// reg2reg. Sent as "YYYY-MM-DD HH:MM:SS" (the pg_payment_date layout), a
	// 24h window. Confirm on the sandbox; a wrong format may reject the payout.
	params.Set("pg_order_time_limit", g.now().UTC().Add(24*time.Hour).Format(paymentDateLayout))
	// TODO(verify): pg_idempotency_key is documented for /g2g/p2p2nonreg but not
	// explicitly for reg2reg; sent anyway — an ignored extra param is harmless,
	// and pg_order_id is unique per payout regardless, which is the real dedupe.
	if req.IdempotencyKey != "" {
		params.Set("pg_idempotency_key", req.IdempotencyKey)
	}
	if g.cfg.TestingMode {
		params.Set("pg_testing_mode", "1")
	}

	// Idempotent:false — a payout dispatch is NEVER auto-retried by the HTTP
	// client. A timeout/5xx therefore surfaces as ErrProviderOutcomeUnknown and
	// the usecase leaves the payout `sent` for the reconciler, never resends.
	values, _, err := g.call(ctx, "reg2reg", pathReg2Reg, params, false)
	if err != nil {
		return nil, err
	}

	providerRef := strings.TrimSpace(values.Get("pg_payment_id"))
	status, known := mapPayoutStatus(values.Get("pg_payment_status"))
	if !known {
		// Envelope pg_status was ok (call() already checked it) but the money
		// status word is absent/unrecognised: accepted, still processing. Never
		// read as paid.
		status = domain.PayoutSent
		g.log.Warn("freedompay payout accepted with unrecognised pg_payment_status, treating as processing",
			slog.String("payout_id", req.PayoutID.String()),
			slog.String("status", values.Get("pg_payment_status")),
		)
	}

	return &domain.GatewayPayout{
		ProviderRef: providerRef,
		Status:      status,
		Amount:      req.Amount,
	}, nil
}

// GetPayout reads a payout's status by OUR order id (/g2g/payout_status2). This
// is the reconciliation path; it is safe to retry, so it IS idempotent at the
// HTTP layer.
func (g *PayoutGateway) GetPayout(ctx context.Context, orderID string) (*domain.GatewayPayout, error) {
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("freedompay payout: empty order id: %w", domain.ErrValidation)
	}
	params := url.Values{}
	params.Set("pg_order_id", orderID)

	values, _, err := g.call(ctx, "payout_status2", pathPayoutStatus, params, true)
	if err != nil {
		return nil, err
	}

	status, known := mapPayoutStatus(values.Get("pg_payment_status"))
	if !known {
		// A signed answer we cannot interpret is UNKNOWN, never "paid": surface
		// it as an unknown outcome so the reconciler leaves the payout `sent`.
		g.log.Warn("freedompay payout_status2 unmapped status",
			slog.String("order_id", orderID),
			slog.String("status", values.Get("pg_payment_status")),
		)
		return nil, fmt.Errorf("freedompay payout_status2: unmapped status %q: %w",
			values.Get("pg_payment_status"), domain.ErrProviderOutcomeUnknown)
	}

	out := &domain.GatewayPayout{
		ProviderRef: strings.TrimSpace(values.Get("pg_payment_id")),
		Status:      status,
	}
	if minor, err := payment.ParseMinor(values.Get("pg_amount")); err == nil && minor > 0 {
		out.Amount = domain.Money{AmountMinor: minor, Currency: domain.CurrencyKZT}
	}
	if status == domain.PayoutFailed {
		out.FailureCode = strings.TrimSpace(values.Get("pg_error_code"))
		out.FailureMessage = sanitise(values.Get("pg_error_description"))
	}
	return out, nil
}

// call signs and performs one payout Sync-API request and returns the
// flattened, signature-verified response. It is the payout twin of
// Gateway.call: both directions are signed, the answer's signature is checked
// here, and a non-ok envelope is a definite rejection.
func (g *PayoutGateway) call(ctx context.Context, op, path string, params url.Values, idempotent bool) (url.Values, json.RawMessage, error) {
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

// mapPayoutStatus translates FreedomPay's payout money-status word into a
// domain PayoutStatus. The second result reports whether the word was
// recognised — an unknown word is NEVER read as paid (money-safety).
//
// TODO(verify): the documented value set is {success, process, error}; the rest
// are defensive synonyms from the acquiring vocabulary and must be confirmed on
// the sandbox.
func mapPayoutStatus(s string) (domain.PayoutStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "ok", "paid", "1":
		return domain.PayoutPaid, true
	case "process", "processing", "pending", "new":
		return domain.PayoutSent, true
	case "error", "failed", "rejected", "declined", "0":
		return domain.PayoutFailed, true
	default:
		return domain.PayoutSent, false
	}
}

func validatePayout(req domain.PayoutRequest) error {
	switch {
	case req.PayoutID == uuid.Nil:
		return fmt.Errorf("freedompay payout: no payout id: %w", domain.ErrValidation)
	case req.Amount.AmountMinor <= 0:
		return fmt.Errorf("freedompay payout: amount must be positive: %w", domain.ErrValidation)
	case !req.Amount.Currency.Valid():
		return fmt.Errorf("freedompay payout: unsupported currency %q: %w", req.Amount.Currency, domain.ErrValidation)
	case req.Method != domain.PayoutMethodFreedomPayCardToken:
		return fmt.Errorf("freedompay payout: unsupported method %q: %w", req.Method, domain.ErrValidation)
	}
	// The card token must be an opaque UUID handle, never a PAN.
	if _, err := uuid.Parse(strings.TrimSpace(req.DestinationToken)); err != nil {
		return fmt.Errorf("freedompay payout: destination token must be a card token, not a PAN: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(req.DestinationCustomerRef) == "" {
		// reg2reg addresses a saved card by (pg_user_id, pg_card_token_to);
		// without the user id we cannot build a safe request and refuse rather
		// than guess.
		return fmt.Errorf("freedompay payout: destination has no provider user id (pg_user_id): %w", domain.ErrValidation)
	}
	return nil
}

// payoutDescription bounds the free-text description sent to the provider.
func payoutDescription(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "BookEat restaurant settlement"
	}
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}
