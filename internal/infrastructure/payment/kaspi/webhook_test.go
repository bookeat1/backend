package kaspi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"backend-core/internal/domain"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const successBody = `{"event":"payment.success","paymentId":991,"type":"qr","status":"Processed",` +
	`"statusDesc":"","amount":2539,"feeAmount":38,"qrToken":null,"receiptUrl":null,` +
	`"orderNumber":null,"timestamp":"2026-08-27T10:00:00.000Z"}`

func TestVerifyWebhookAcceptsAProperlySignedSuccess(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})

	ev, err := gw.VerifyWebhook([]byte(successBody), map[string]string{
		SignatureHeader: sign(testSecret, successBody),
	})
	if err != nil {
		t.Fatalf("VerifyWebhook() error = %v", err)
	}
	// Kaspi is one-stage, but the state machine has no created→captured edge:
	// success enters as `authorized` and the pre-order capture follows
	// immediately in the usecase layer.
	if ev.Type != domain.WebhookPaymentAuthorized {
		t.Fatalf("type = %s, want %s", ev.Type, domain.WebhookPaymentAuthorized)
	}
	if ev.ProviderPaymentID != "991" {
		t.Fatalf("provider payment id = %q", ev.ProviderPaymentID)
	}
	// The service's OWN delivery idempotency key, verbatim: its retries and
	// our (provider, provider_event_id) uniqueness must agree on what "the
	// same delivery" is.
	if ev.ProviderEventID != "qr:991:payment.success" {
		t.Fatalf("event id = %q, want qr:991:payment.success", ev.ProviderEventID)
	}
	// Tenge on the wire, tiyn in the domain.
	if ev.Amount.AmountMinor != 253_900 {
		t.Fatalf("amount = %d minor, want 253900", ev.Amount.AmountMinor)
	}
	if !ev.SignatureValid {
		t.Fatal("signature not marked valid")
	}
}

func TestVerifyWebhookRejectsABadOrMissingSignature(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})

	for name, headers := range map[string]map[string]string{
		"no signature":    {},
		"empty":           {SignatureHeader: ""},
		"wrong secret":    {SignatureHeader: sign("not-our-secret", successBody)},
		"not hex":         {SignatureHeader: "sha256=zzzz"},
		"another payload": {SignatureHeader: sign(testSecret, `{"event":"payment.failed"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			ev, err := gw.VerifyWebhook([]byte(successBody), headers)
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatalf("error = %v, want ErrUnauthorized", err)
			}
			if ev != nil {
				t.Fatal("an unverified payload must not be interpreted at all")
			}
		})
	}
}

func TestVerifyWebhookAcceptsAnySecretConfiguredForAnyCompany(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})
	gw.cfg.WebhookSecrets = map[string]string{"7": "secret-seven", "8": "secret-eight"}

	for _, secret := range []string{"secret-seven", "secret-eight"} {
		if _, err := gw.VerifyWebhook([]byte(successBody), map[string]string{
			SignatureHeader: sign(secret, successBody),
		}); err != nil {
			t.Fatalf("secret %q rejected: %v", secret, err)
		}
	}
	if _, err := gw.VerifyWebhook([]byte(successBody), map[string]string{
		SignatureHeader: sign("secret-nine", successBody),
	}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("an unconfigured company's signature was accepted: %v", err)
	}
}

func TestVerifyWebhookNeverRoundsAnUnknownOutcomeToEitherSide(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})
	body := `{"event":"payment.lost","paymentId":991,"type":"qr","status":"SessionExpired","amount":2539}`

	ev, err := gw.VerifyWebhook([]byte(body), map[string]string{SignatureHeader: sign(testSecret, body)})
	if err != nil {
		t.Fatalf("VerifyWebhook() error = %v", err)
	}
	// "lost" means the service could not find out what happened. Read as
	// failed it would tell a guest their money is safe; read as paid it would
	// feed a kitchen for free. It is stored and left alone.
	if ev.Type != domain.WebhookUnknown {
		t.Fatalf("type = %s, want %s", ev.Type, domain.WebhookUnknown)
	}
}

func TestVerifyWebhookMapsTheFailureAndExpiryEvents(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})

	cases := map[string]struct {
		body string
		want domain.WebhookEventType
	}{
		"guest never paid, link died": {
			body: `{"event":"payment.expired","paymentId":992,"type":"qr","status":"Expired","statusDesc":"Время оплаты истекло"}`,
			want: domain.WebhookPaymentExpired,
		},
		"guest declined in the app": {
			body: `{"event":"payment.failed","paymentId":993,"type":"qr","status":"CancelledByUser"}`,
			want: domain.WebhookPaymentFailed,
		},
		"an event this build does not know": {
			body: `{"event":"payment.teleported","paymentId":994,"type":"qr","status":"Processed"}`,
			want: domain.WebhookUnknown,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev, err := gw.VerifyWebhook([]byte(tc.body), map[string]string{SignatureHeader: sign(testSecret, tc.body)})
			if err != nil {
				t.Fatalf("VerifyWebhook() error = %v", err)
			}
			if ev.Type != tc.want {
				t.Fatalf("type = %s, want %s", ev.Type, tc.want)
			}
		})
	}
}

func TestVerifyWebhookReadsTheSignatureHeaderWhateverItsCasing(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})
	for _, name := range []string{"x-webhook-signature", "X-Webhook-Signature", "X-WEBHOOK-SIGNATURE"} {
		if _, err := gw.VerifyWebhook([]byte(successBody), map[string]string{name: sign(testSecret, successBody)}); err != nil {
			t.Fatalf("header %q rejected: %v", name, err)
		}
	}
}
