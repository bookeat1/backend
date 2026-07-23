package freedompay

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// ScriptNameHeader lets the transport layer tell the adapter which of our own
// callback routes received the message, because the script name is part of the
// signature (see signature.go). It is optional: when absent,
// Config.ResultScriptName is used.
//
// It is OUR header, set by OUR handler from the matched route — a caller cannot
// use it to make an unsigned payload verify, since changing the script name
// only ever changes what the signature must be.
const ScriptNameHeader = "X-Bookeat-Script-Name"

// ErrSignature is returned when a callback is unsigned or wrongly signed. The
// payload is not interpreted in that case (spec §7).
var ErrSignature = fmt.Errorf("freedompay: webhook signature verification failed: %w", domain.ErrUnauthorized)

// VerifyWebhook validates pg_sig FIRST and only then reads the callback.
//
// The message is a form POST; the signature covers the script name of OUR
// result_url plus every field of the message sorted by name plus the secret
// key (MD5, hex). Comparison is constant-time.
//
// Fields we do not know about still participate in the signature — that is why
// the whole url.Values is signed rather than a fixed struct. An attacker adding
// an extra field therefore breaks the signature instead of sneaking a value
// past us.
func (g *Gateway) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body: %w", ErrSignature)
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("freedompay: webhook body: %w", payment.ErrProviderMalformed)
	}

	script := strings.TrimSpace(header(headers, ScriptNameHeader))
	if script == "" {
		script = g.cfg.ResultScriptName
	} else {
		script = scriptName(script)
	}
	if !verify(script, values, g.cfg.SecretKey) {
		return nil, ErrSignature
	}

	captured := isTrue(values.Get("pg_captured"))
	result := strings.TrimSpace(values.Get("pg_result"))

	// pg_result is the callback's own verdict: 1 = the payment went through,
	// 0 = it did not. It outranks pg_payment_status, which is not always
	// present on the callback.
	var status domain.PaymentStatus
	switch {
	case result == "0":
		status = domain.PaymentFailed
	case result == "1" && captured:
		status = domain.PaymentCaptured
	case result == "1":
		status = domain.PaymentAuthorized
	default:
		// TODO(verify): confirm on the sandbox that pg_result is always present
		// on result_url callbacks. Falling back to pg_payment_status keeps an
		// unexpected shape from being read as "paid".
		var known bool
		status, known = mapPaymentStatus(values.Get("pg_payment_status"), captured)
		if !known {
			g.log.Warn("freedompay unmapped callback status",
				slog.String("pg_result", result),
				slog.String("pg_payment_status", values.Get("pg_payment_status")),
			)
		}
	}

	eventType := domain.WebhookUnknown
	switch status {
	case domain.PaymentAuthorized:
		eventType = domain.WebhookPaymentAuthorized
	case domain.PaymentCaptured:
		eventType = domain.WebhookPaymentCaptured
	case domain.PaymentFailed:
		eventType = domain.WebhookPaymentFailed
	case domain.PaymentVoided:
		eventType = domain.WebhookPaymentVoided
	case domain.PaymentExpired:
		eventType = domain.WebhookPaymentExpired
	case domain.PaymentRefunded, domain.PaymentPartiallyRefunded:
		eventType = domain.WebhookRefundSucceeded
	}

	amountMinor, _ := payment.ParseMinor(values.Get("pg_amount"))
	currency := domain.Currency(strings.ToUpper(strings.TrimSpace(values.Get("pg_currency"))))
	if !currency.Valid() {
		currency = domain.CurrencyKZT
	}

	ev := &domain.WebhookEvent{
		Provider:          domain.ProviderFreedomPay,
		ProviderPaymentID: strings.TrimSpace(values.Get("pg_payment_id")),
		MerchantPaymentID: strings.TrimSpace(values.Get("pg_order_id")),
		Type:              eventType,
		Status:            status,
		Amount:            domain.Money{AmountMinor: amountMinor, Currency: currency},
		SignatureValid:    true,
		Payload:           redactedPayload(values),
	}
	if at := parsePaymentDate(values.Get("pg_payment_date")); at != nil {
		ev.OccurredAt = *at
	}
	if status == domain.PaymentFailed {
		ev.FailureCode = strings.TrimSpace(values.Get("pg_failure_code"))
		if ev.FailureCode == "" {
			ev.FailureCode = strings.TrimSpace(values.Get("pg_error_code"))
		}
		ev.FailureMessage = sanitise(values.Get("pg_failure_description"))
	}

	// FreedomPay sends no event id and retries the same callback every 30
	// minutes for 2 hours, so the id is derived and must be stable across those
	// retries: pg_salt and pg_sig change per delivery, the facts below do not.
	ev.ProviderEventID = strings.Join([]string{
		"result", ev.ProviderPaymentID, result, boolDigit(captured),
	}, ":")

	return ev, nil
}

// CanReject reports whether this callback allows us to refuse the payment
// (pg_can_reject=1). It is the saga compensation hook from spec §6: if the
// booking could not be created, answering `rejected` makes FreedomPay reverse
// the payment without a separate API call.
//
// It reads the VERIFIED event's stored payload, so a caller cannot accidentally
// consult an unauthenticated body.
func CanReject(verified *domain.WebhookEvent) bool {
	if verified == nil || len(verified.Payload) == 0 {
		return false
	}
	var fields map[string]string
	if err := json.Unmarshal(verified.Payload, &fields); err != nil {
		return false
	}
	return isTrue(fields["pg_can_reject"])
}

// AckOK is the signed XML answer that accepts a callback.
func (g *Gateway) AckOK(description string) []byte { return g.ack("ok", description) }

// AckRejected refuses a payment. Only legal when the incoming callback carried
// pg_can_reject=1; otherwise FreedomPay treats the payment as completed no
// matter what we answer.
func (g *Gateway) AckRejected(description string) []byte { return g.ack("rejected", description) }

// AckError tells FreedomPay we could not interpret the message, which makes it
// retry.
func (g *Gateway) AckError(description string) []byte { return g.ack("error", description) }

func (g *Gateway) ack(status, description string) []byte {
	params := url.Values{}
	params.Set("pg_status", status)
	if description != "" {
		params.Set("pg_description", description)
	}
	params.Set(saltParam, newSalt())
	params.Set(sigParam, sign(g.cfg.ResultScriptName, params, g.cfg.SecretKey))

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><response>`)
	for _, k := range []string{"pg_status", "pg_description", saltParam, sigParam} {
		v := params.Get(k)
		if v == "" {
			continue
		}
		b.WriteString("<" + k + ">")
		_ = xmlEscape(&b, v)
		b.WriteString("</" + k + ">")
	}
	b.WriteString(`</response>`)
	return []byte(b.String())
}

func xmlEscape(b *strings.Builder, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	_, err := b.WriteString(r.Replace(s))
	return err
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// header looks a header up case-insensitively; the map from transport may use
// any casing.
func header(headers map[string]string, name string) string {
	if headers == nil {
		return ""
	}
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
