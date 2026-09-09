package kaspi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// SignatureHeader carries the HMAC the Kaspi service signs every delivery
// with: "sha256=<hex of HMAC-SHA256(raw body, webhook secret)>".
const SignatureHeader = "X-Webhook-Signature"

// ErrSignature is returned when a callback is unsigned or the signature does
// not match. The payload is NOT interpreted in that case (spec §7): the caller
// records the event with signature_valid=false and answers 401.
var ErrSignature = fmt.Errorf("kaspi: webhook signature verification failed: %w", domain.ErrUnauthorized)

// webhookPayload is the delivery shape of the Kaspi service (src/tracker.js,
// buildPayload). Amounts on this wire are whole TENGE, not tiyn.
type webhookPayload struct {
	Event      string      `json:"event"`
	PaymentID  json.Number `json:"paymentId"`
	Type       string      `json:"type"`
	Status     string      `json:"status"`
	StatusDesc string      `json:"statusDesc"`
	Amount     json.Number `json:"amount"`
	Timestamp  string      `json:"timestamp"`
}

// events maps the service's event names onto the domain's closed set.
//
// The two entries worth arguing about:
//
//   - payment.success → WebhookPaymentAuthorized, NOT …Captured. Kaspi is
//     one-stage, but `created → captured` is not a legal transition; the
//     usecase layer takes a pre-order from `authorized` to `captured` itself
//     the instant it is authorized (PaymentPurpose.CapturesImmediately), and
//     routing through it keeps the ledger and the idempotency identical to
//     every other acquirer. See the package doc.
//   - payment.lost → WebhookUnknown. "Lost" is the service saying it could
//     NOT find out what happened (the cashier session died mid-flight). That
//     is the one thing a payment system must never round to either side: it
//     is stored as evidence and left for reconciliation, never read as failed
//     (which would tell a guest their money is safe) and never as paid.
var events = map[string]domain.WebhookEventType{
	"payment.success": domain.WebhookPaymentAuthorized,
	"payment.failed":  domain.WebhookPaymentFailed,
	"payment.expired": domain.WebhookPaymentExpired,
	"payment.lost":    domain.WebhookUnknown,
}

// VerifyWebhook checks the HMAC first and only then reads the payload.
//
// A callback does not say which company sent it, so every configured secret
// is tried (see Config.WebhookSecrets); a body that matches any of them was
// signed by a company we configured. Every comparison is constant-time and the
// loop is not short-circuited on the first mismatch — with a handful of
// secrets the cost is irrelevant and the timing tells an attacker nothing.
func (g *Gateway) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	if !g.signatureMatches(raw, header(headers, SignatureHeader)) {
		return nil, ErrSignature
	}

	var p webhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("kaspi: webhook body: %w", payment.ErrProviderMalformed)
	}
	paymentID := strings.TrimSpace(p.PaymentID.String())
	if paymentID == "" {
		return nil, fmt.Errorf("kaspi: webhook carries no paymentId: %w", payment.ErrProviderMalformed)
	}

	kind, known := events[strings.TrimSpace(p.Event)]
	if !known {
		// Authentic, but not a meaning this build acts on. Stored, never
		// guessed at (spec §7).
		kind = domain.WebhookUnknown
	}

	txType := strings.TrimSpace(p.Type)
	if txType == "" {
		txType = "qr"
	}

	event := &domain.WebhookEvent{
		Provider: domain.ProviderKaspi,
		// The Kaspi service's OWN delivery idempotency key
		// (src/tracker.js: `${payment_type}:${payment_id}:${event}`). Reusing
		// it verbatim means its retries and our (provider, provider_event_id)
		// uniqueness agree on what "the same delivery" is, so a redelivered
		// success can never be applied twice.
		ProviderEventID:   txType + ":" + paymentID + ":" + strings.TrimSpace(p.Event),
		ProviderPaymentID: paymentID,
		// Kaspi's QR create call accepts no merchant order reference, so a
		// callback can only be resolved by the acquirer-side id we stored at
		// Authorize time. Deliberately empty rather than a guess.
		MerchantPaymentID: "",
		Type:              kind,
		OccurredAt:        parseKaspiTime(p.Timestamp),
		SignatureValid:    true,
		Payload:           append(json.RawMessage(nil), raw...),
	}
	if status, ok := mapQrStatus(p.Status); ok {
		event.Status = status
	}
	if minor, err := fromTenge(p.Amount); err == nil && minor > 0 {
		event.Amount = domain.Money{AmountMinor: minor, Currency: domain.CurrencyKZT}
	}
	if kind == domain.WebhookPaymentFailed || kind == domain.WebhookPaymentExpired {
		event.FailureCode = strings.TrimSpace(p.Status)
		event.FailureMessage = sanitise(p.StatusDesc)
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = g.now()
	}
	return event, nil
}

// signatureMatches reports whether raw was signed by any configured secret.
func (g *Gateway) signatureMatches(raw []byte, provided string) bool {
	provided = strings.TrimSpace(provided)
	if provided == "" || len(g.cfg.WebhookSecrets) == 0 {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(provided, "sha256="))
	if err != nil || len(got) == 0 {
		return false
	}
	matched := false
	for _, secret := range g.cfg.WebhookSecrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		if hmac.Equal(got, mac.Sum(nil)) {
			matched = true
		}
	}
	return matched
}

// header reads a header case-insensitively: the transport layer hands us a
// plain map, whose keys may or may not be canonical.
func header(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	if v, ok := headers[http.CanonicalHeaderKey(name)]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
