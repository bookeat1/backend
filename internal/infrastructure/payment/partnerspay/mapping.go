package partnerspay

import (
	"strings"

	"backend-core/internal/domain"
)

// This file is the anti-corruption layer (spec §2), same role as
// freedompay/mapping.go and tiptoppay/mapping.go: every Partners Pay word is
// meant to be translated here and nowhere else, so no provider-specific
// status string ever escapes this package.
//
// TODO(contract): Partners Pay's status vocabulary is completely unknown — no
// literal value below is a real one. See
// docs/payments/partnerspay-integration-questions.md, group "Status
// dictionary", for what to ask. Until answered, both functions below
// recognise NOTHING and report "unknown" for every input; that is
// deliberate, not a placeholder bug: it is the one property this file must
// never lose.

// mapPaymentStatus translates a raw Partners Pay payment status string into a
// domain.PaymentStatus. The second result reports whether the value was
// recognised.
//
// The non-negotiable rule, enforced by TestMapPaymentStatusNeverReadsUnknownAsPaid:
// an unrecognised (or empty) status is NEVER translated to PaymentCaptured or
// PaymentAuthorized — it falls back to PaymentCreated with known=false, the
// same fail-safe direction freedompay.mapPaymentStatus and
// tiptoppay.mapStatus use for the same reason: guessing "paid" from a word we
// do not understand is how money goes missing unnoticed. Callers must log the
// unrecognised value (see freedompay.Gateway.status's
// "freedompay unmapped payment status" for the pattern) rather than silently
// swallow it.
func mapPaymentStatus(raw string) (domain.PaymentStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	// TODO(contract): add real cases here once Partners Pay's status
	// dictionary is confirmed (e.g. whether it distinguishes a hold from a
	// capture the way FreedomPay's pg_payment_status + pg_captured pair does,
	// or reports one combined status word the way TipTopPay's
	// AwaitingAuthentication / Authorized / Completed / Cancelled / Declined
	// do).
	default:
		return domain.PaymentCreated, false
	}
}

// mapRefundStatus translates a raw Partners Pay refund status string into a
// domain.RefundStatus, with the same fail-safe rule: unknown never reads as
// succeeded.
func mapRefundStatus(raw string) (domain.RefundStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	// TODO(contract): add real cases once confirmed.
	default:
		return domain.RefundFailed, false
	}
}
