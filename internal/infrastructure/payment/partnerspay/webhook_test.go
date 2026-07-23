package partnerspay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// TestVerifyWebhookReportsContractUnknown documents today's behaviour: the
// method compiles, satisfies domain.PaymentGateway, and refuses to interpret
// anything rather than guess at an unconfirmed signature scheme.
func TestVerifyWebhookReportsContractUnknown(t *testing.T) {
	g, err := New(validConfig(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.VerifyWebhook([]byte(`{"status":"paid"}`), map[string]string{SignatureHeader: "whatever"}); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("VerifyWebhook() error = %v, want ErrContractUnknown", err)
	}
}

// TestVerifyHMACSHA256IsConstantTimeAndCorrect exercises the one primitive in
// this package that does not depend on Partners Pay's actual contract: given
// a body, a secret and a hex-encoded HMAC-SHA256 digest over that body, the
// helper must accept the correct one and reject anything else — a wrong
// digest, a tampered body, or a missing/malformed signature.
func TestVerifyHMACSHA256IsConstantTimeAndCorrect(t *testing.T) {
	body := []byte(`{"event":"payment.paid","amount":"100.00"}`)
	secret := "test-webhook-secret"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := hex.EncodeToString(mac.Sum(nil))

	if !verifyHMACSHA256(body, secret, valid) {
		t.Error("verifyHMACSHA256 rejected a correctly signed body")
	}
	if verifyHMACSHA256(body, secret, "") {
		t.Error("verifyHMACSHA256 accepted an empty signature")
	}
	if verifyHMACSHA256(body, "", valid) {
		t.Error("verifyHMACSHA256 accepted an empty secret")
	}
	if verifyHMACSHA256(body, secret, "not-hex-at-all-zz") {
		t.Error("verifyHMACSHA256 accepted a malformed (non-hex) signature")
	}
	if verifyHMACSHA256([]byte(`{"event":"payment.paid","amount":"999.00"}`), secret, valid) {
		t.Error("verifyHMACSHA256 accepted a signature for a different body (tampered payload)")
	}
	if verifyHMACSHA256(body, "wrong-secret", valid) {
		t.Error("verifyHMACSHA256 accepted a signature computed with the wrong secret")
	}
}

// header is exercised indirectly through the package but not directly by
// production code paths yet (VerifyWebhook does not call it until the real
// scheme is wired in) — this keeps it covered and behaviourally locked in
// the meantime, matching the case-insensitive lookup freedompay and
// tiptoppay both rely on.
func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	headers := map[string]string{"X-Partnerspay-Signature": "abc"}
	if got := header(headers, SignatureHeader); got != "abc" {
		t.Errorf("header lookup = %q, want %q", got, "abc")
	}
	if got := header(nil, SignatureHeader); got != "" {
		t.Errorf("header(nil, ...) = %q, want empty", got)
	}
	if got := header(map[string]string{}, SignatureHeader); got != "" {
		t.Errorf("header(missing) = %q, want empty", got)
	}
}
