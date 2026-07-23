package freedompay

import (
	"encoding/xml"
	"io"
	"net/url"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// This file is the anti-corruption layer (spec §2). Every FreedomPay word is
// translated here and nowhere else; no pg_* string escapes this package.

// Envelope-level status of any Sync API answer.
const (
	statusOK    = "ok"
	statusError = "error"
)

// pg_payment_status values.
//
// TODO(verify): only "success" is attested by the documentation example for
// /g2g/status_v2. The rest are the historical PayBox vocabulary and MUST be
// confirmed on the sandbox — drive a payment through hold → clearing → refund →
// cancel and record the literal pg_payment_status at each step. An unrecognised
// value is deliberately mapped to `created` and logged, never to `captured`:
// guessing "paid" from a word we do not know is how money goes missing.
const (
	paymentStatusNew       = "new"
	paymentStatusPending   = "pending"
	paymentStatusProcess   = "process"
	paymentStatusSuccess   = "success"
	paymentStatusPartial   = "partial"
	paymentStatusFailed    = "failed"
	paymentStatusError     = "error"
	paymentStatusRejected  = "rejected"
	paymentStatusRevoked   = "revoked"
	paymentStatusCancelled = "canceled"
	paymentStatusRefunded  = "refunded"
	paymentStatusExpired   = "expired"
)

// mapPaymentStatus translates pg_payment_status (+ pg_captured, which
// distinguishes a hold from a completed charge in the two-stage flow) into a
// domain status. The second result reports whether the word was recognised.
//
//	pg_payment_status   pg_captured   domain
//	new / pending / process   any     created
//	success                   0       authorized   (funds held, not cleared)
//	success                   1       captured
//	partial                   any     partially_refunded  TODO(verify)
//	refunded                  any     refunded
//	revoked / canceled        any     voided
//	failed / error / rejected any     failed
//	expired                   any     expired
func mapPaymentStatus(status string, captured bool) (domain.PaymentStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case paymentStatusNew, paymentStatusPending, paymentStatusProcess:
		return domain.PaymentCreated, true
	case paymentStatusSuccess, statusOK:
		if captured {
			return domain.PaymentCaptured, true
		}
		return domain.PaymentAuthorized, true
	case paymentStatusPartial:
		return domain.PaymentPartiallyRefunded, true
	case paymentStatusRefunded:
		return domain.PaymentRefunded, true
	case paymentStatusRevoked, paymentStatusCancelled:
		return domain.PaymentVoided, true
	case paymentStatusFailed, paymentStatusError, paymentStatusRejected:
		return domain.PaymentFailed, true
	case paymentStatusExpired:
		return domain.PaymentExpired, true
	default:
		return domain.PaymentCreated, false
	}
}

// mapOperationStatus translates the per-operation result words
// (pg_revoke_status, pg_refund_status) into a boolean success.
//
// TODO(verify): "success" is the only value the documentation shows. Confirm
// on the sandbox whether a pending/asynchronous value exists — if it does, a
// refund must be recorded as `created` rather than `succeeded` until the
// callback lands.
func mapOperationStatus(s string) (domain.RefundStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "ok", "1":
		return domain.RefundSucceeded, true
	case "pending", "process", "processing":
		// Non-blocking item #7 (second review): these words mean the
		// acquirer accepted the request but has not confirmed the outcome
		// yet — an in-progress, unknown-until-resolved state, exactly what
		// domain.RefundPending exists for. Mapping them to RefundCreated was
		// wrong: RefundStatus.RetryableAtAcquirer() reports `created` as
		// safe to call Refund again from scratch (it never reached the
		// acquirer), which is false here — this call DID reach the
		// acquirer and may yet complete as a real refund.
		return domain.RefundPending, true
	case "", "failed", "error", "rejected", "0":
		return domain.RefundFailed, true
	default:
		return domain.RefundFailed, false
	}
}

// paymentDateLayout is the pg_payment_date format: "2024-11-28 15:44:53".
// TODO(verify): the documentation does not state the time zone of
// pg_payment_date (pg_datetime next to it is RFC3339 with an offset). Treated
// as UTC here; check on the sandbox against a payment made at a known moment.
const paymentDateLayout = "2006-01-02 15:04:05"

func parsePaymentDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		u := t.UTC()
		return &u
	}
	t, err := time.ParseInLocation(paymentDateLayout, s, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}

// decodeXML flattens a FreedomPay XML answer into url.Values.
//
// A flat map rather than a struct per endpoint, for two reasons: the signature
// is computed over ALL fields of the message including ones the documentation
// does not list ("either party may add additional parameters… these parameters
// also participate in the signature calculation"), and a struct would silently
// drop them and break verification.
//
// TODO(verify): nested elements are flattened to their leaf tag name. No
// response we consume is nested today; a fiscalised merchant's receipt payload
// would be, and would need the recursive rule from the documentation.
func decodeXML(body []byte) (url.Values, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	out := url.Values{}

	var depth int
	var name string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth >= 2 {
				name = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if depth >= 2 {
				text.Write(t)
			}
		case xml.EndElement:
			if depth >= 2 && name == t.Name.Local {
				out.Add(name, strings.TrimSpace(text.String()))
				name = ""
				text.Reset()
			}
			depth--
		}
	}
	return out, nil
}
