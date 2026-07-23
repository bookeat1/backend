package tiptoppay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// NotificationTypeHeader tells the adapter which notification arrived.
//
// TipTopPay does not put the type in the body: each of check / pay / fail /
// confirm / refund / cancel is delivered to its own configured URL. The REST
// layer therefore mounts one route per type
// (POST /webhooks/payments/tiptoppay/{type}) and sets this header from the path
// before calling VerifyWebhook. It is OUR header, set by OUR handler — an
// attacker cannot use it to change the meaning of a payload they cannot sign.
const NotificationTypeHeader = "X-Bookeat-Notification-Type"

// Signature headers, both sent by TipTopPay. They differ only in whether the
// signed bytes were URL-encoded; whichever matches the body we actually
// received is the right one.
const (
	headerContentHMAC  = "Content-HMAC"
	headerXContentHMAC = "X-Content-HMAC"
)

// ErrSignature is returned when a callback is unsigned or the signature does
// not match. The payload is NOT interpreted in that case (spec §7): the caller
// records the event with signature_valid=false and answers 401.
var ErrSignature = fmt.Errorf("tiptoppay: webhook signature verification failed: %w", domain.ErrUnauthorized)

// VerifyWebhook checks the HMAC first and only then reads the payload.
//
// Signature: base64(HMAC-SHA256(raw body, API Secret)), UTF-8, delivered in
// Content-HMAC and X-Content-HMAC. Comparison is constant-time (hmac.Equal).
//
// Order matters and is the whole point of this function: a payload that has not
// been authenticated is attacker-controlled data, and parsing it before the
// check would already be acting on it.
func (g *Gateway) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	if err := g.verifySignature(raw, headers); err != nil {
		return nil, err
	}

	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("tiptoppay: webhook body: %w", payment.ErrProviderMalformed)
	}

	kind := strings.ToLower(strings.TrimSpace(header(headers, NotificationTypeHeader)))
	if kind == "" {
		// Without the route we cannot tell `pay` from `confirm`. Refusing to
		// guess is the safe answer: the event is stored as authentic but
		// unclassified and goes to manual review (spec §7).
		kind = "unknown"
	}

	status, known := mapStatus(values.Get("Status"))
	if !known {
		status = statusForNotification(kind, domain.PaymentCreated)
	}

	amountMinor, _ := payment.ParseMinor(values.Get("Amount"))
	currency := domain.Currency(values.Get("Currency"))
	if !currency.Valid() {
		currency = domain.CurrencyKZT
	}

	txID := strings.TrimSpace(values.Get("TransactionId"))
	eventType := mapNotification(kind, status)

	ev := &domain.WebhookEvent{
		Provider:          domain.ProviderTipTopPay,
		ProviderPaymentID: txID,
		MerchantPaymentID: strings.TrimSpace(values.Get("InvoiceId")),
		Type:              eventType,
		Status:            status,
		Amount:            domain.Money{AmountMinor: amountMinor, Currency: currency},
		OccurredAt:        parseNotificationTime(values.Get("DateTime")),
		SignatureValid:    true,
		Payload:           redactedPayload(values),
	}

	// A refund notification is about the refund's OWN transaction; the payment
	// it belongs to is PaymentTransactionId.
	if eventType == domain.WebhookRefundSucceeded {
		ev.ProviderRefundID = txID
		if orig := strings.TrimSpace(values.Get("PaymentTransactionId")); orig != "" {
			ev.ProviderPaymentID = orig
		}
	}

	if eventType == domain.WebhookPaymentFailed {
		ev.FailureCode = strings.TrimSpace(values.Get("ReasonCode"))
		ev.FailureMessage = sanitise(values.Get("Reason"))
	}

	ev.ProviderEventID = eventID(kind, ev)
	return ev, nil
}

func (g *Gateway) verifySignature(raw []byte, headers map[string]string) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty body: %w", ErrSignature)
	}

	mac := hmac.New(sha256.New, []byte(g.cfg.APISecret))
	mac.Write(raw)
	want := []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	var present bool
	for _, name := range []string{headerContentHMAC, headerXContentHMAC} {
		got := strings.TrimSpace(header(headers, name))
		if got == "" {
			continue
		}
		present = true
		// hmac.Equal is constant-time and length-safe.
		if hmac.Equal([]byte(got), want) {
			return nil
		}
	}
	if !present {
		return fmt.Errorf("no signature header: %w", ErrSignature)
	}
	return ErrSignature
}

// eventID builds the idempotency key for payment_events.
//
// TipTopPay sends no event id, and it retries notifications, so the id has to
// be derived and stable across retries of the SAME event: type plus the
// transaction it concerns. Two different events about one transaction (pay then
// confirm) get different ids; a retry of one of them gets the same id.
func eventID(kind string, ev *domain.WebhookEvent) string {
	id := ev.ProviderPaymentID
	if ev.ProviderRefundID != "" {
		id = ev.ProviderRefundID
	}
	if id == "" {
		id = ev.MerchantPaymentID
	}
	return kind + ":" + id
}

// redactedPayload turns the callback into JSON for payment_events with card
// data removed. Storing CardFirstSix / CardLastFour / CardExpDate / Token would
// put card identifiers in a table we read in support tickets; the domain never
// needs them (spec §8).
func redactedPayload(values url.Values) json.RawMessage {
	const redacted = "[redacted]"
	sensitive := map[string]struct{}{
		"CardFirstSix": {}, "CardLastFour": {}, "CardExpDate": {}, "CardType": {},
		"CardId": {}, "Token": {}, "CardHolderMessage": {}, "Name": {},
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

// header looks a header up case-insensitively. The map handed to VerifyWebhook
// comes from transport and may use any casing.
func header(headers map[string]string, name string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[name]; ok {
		return v
	}
	canonical := http.CanonicalHeaderKey(name)
	if v, ok := headers[canonical]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// AckBody is the answer TipTopPay expects from every notification endpoint:
// {"code":0} means "registered". Anything else makes it retry.
func AckBody() []byte { return []byte(`{"code":0}`) }
