package partnerspay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"backend-core/internal/domain"
)

// This file is a SHAPE-ONLY scaffold. Nothing about Partners Pay's webhook
// format is confirmed — see
// docs/payments/partnerspay-integration-questions.md, group "Webhooks", for
// the full list of open questions:
//
//   - transport and encoding (JSON body? form-encoded, like FreedomPay's
//     result_url? something else) and the field names it carries;
//   - the signature scheme: which header(s) carry it, what is actually
//     signed (the raw body, like TipTopPay's Content-HMAC; or a
//     script-name-plus-sorted-fields construction, like FreedomPay's pg_sig;
//     these two adapters already disagree with each other, so Partners Pay
//     matching either one is a guess, not a fact) and the hash algorithm
//     (HMAC-SHA256 below is a PLACEHOLDER, not a confirmed choice);
//   - retry behaviour on a non-200 answer, and for how long — this decides
//     how strictly VerifyWebhook + its caller must be idempotent
//     (payment_events already gives us that regardless of provider, but the
//     retry window shapes reconciliation-worker timing, see spec §5);
//   - an event id we can use for payment_events' idempotency key, or whether
//     one has to be derived from stable fields the way both existing
//     adapters do (freedompay.VerifyWebhook: "result:<payment_id>:<result>:
//     <captured>"; tiptoppay.eventID: "<kind>:<transaction_id>").
//
// SignatureHeader is an UNCONFIRMED GUESS at a header name, following the
// common "X-<Provider>-Signature" convention. TODO(contract): replace with
// the real header name(s) Partners Pay sends — do not ship this guess to
// production without checking it against an actual callback.
const SignatureHeader = "X-PartnersPay-Signature" // TODO(contract): unconfirmed guess, verify against a real callback.

// ErrSignature is returned when a callback is unsigned or its signature does
// not verify. The payload must not be interpreted in that case (spec §7) —
// this is the one rule that is NOT waiting on the contract: whatever the real
// scheme turns out to be, "verify before you read" does not change.
var ErrSignature = fmt.Errorf("partnerspay: webhook signature verification failed: %w", domain.ErrUnauthorized)

// VerifyWebhook is not implemented: the request/response shape and the
// signature scheme are both unknown (see the package-level comment above).
// It still satisfies domain.PaymentGateway so the adapter compiles and can be
// wired into the registry the moment credentials exist, without anyone having
// to touch call sites again.
//
// TODO(contract): once the real shape is known, replace this body with:
//  1. verifySignatureConstantTime (or whatever the real algorithm needs) on
//     the RAW bytes, matching whichever header(s) Partners Pay actually sends
//     — see verifyHMACSHA256 below for a ready, constant-time primitive if
//     the scheme turns out to be HMAC-SHA256 over the raw body;
//  2. only then parse raw and translate it into a *domain.WebhookEvent via
//     mapPaymentStatus / mapRefundStatus in mapping.go, following the same
//     "verify first, interpret second" order freedompay.VerifyWebhook and
//     tiptoppay.VerifyWebhook already enforce.
func (g *Gateway) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	return nil, fmt.Errorf("partnerspay: VerifyWebhook: %w", ErrContractUnknown)
}

// verifyHMACSHA256 is a constant-time signature check: computed = HMAC-SHA256
// over raw using secret, hex-encoded, compared against providedHex.
//
// It exists now, ready to be wired into VerifyWebhook, because writing the
// constant-time comparison correctly is the one part of this file that does
// NOT depend on Partners Pay's contract — hmac.Equal (which is
// subtle.ConstantTimeCompare under the hood) is the right tool regardless of
// which header carries the value or what exactly gets signed. Using it later
// means whoever fills in the real Authorize/webhook body cannot accidentally
// regress to a naive == comparison, which leaks timing information about how
// close a forged signature is — the same class of mistake
// freedompay.verify and tiptoppay.verifySignature both go out of their way to
// avoid.
//
// TODO(contract): confirm the algorithm is HMAC-SHA256 over the raw body (not,
// say, HMAC-SHA1, not a FreedomPay-style "script;sorted fields;secret"
// construction, not base64 instead of hex) before wiring this in for real.
func verifyHMACSHA256(raw []byte, secret, providedHex string) bool {
	providedHex = strings.TrimSpace(providedHex)
	if providedHex == "" || secret == "" {
		return false
	}
	provided, err := hex.DecodeString(providedHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	return hmac.Equal(mac.Sum(nil), provided)
}

// header looks a header up case-insensitively, same helper as
// freedompay/webhook.go and tiptoppay/webhook.go — kept local rather than
// shared because each adapter package is meant to be self-contained (spec
// §2: "adding a third acquirer must not require touching a single line" in
// the other two).
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
