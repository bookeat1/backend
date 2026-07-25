package bootstrap

import (
	"testing"

	"backend-core/internal/domain"
)

// The per-provider refund rate is parsed by hand (not through the getEnv*
// helpers) precisely because its failure modes are money-shaped, so its parser
// gets its own tests.
func TestRefundAcquiringByProvider(t *testing.T) {
	t.Run("unset providers are absent, not zero", func(t *testing.T) {
		got, err := refundAcquiringByProvider()
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Fatalf("map = %v, want empty (nothing configured)", got)
		}
	})

	t.Run("an explicit zero is kept", func(t *testing.T) {
		t.Setenv("PAYMENTS_REFUND_ACQUIRING_BPS_FREEDOMPAY", "0")

		got, err := refundAcquiringByProvider()
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		bps, ok := got["freedompay"]
		if !ok {
			t.Fatalf("map = %v, want a freedompay entry (an explicit 0 is a real setting)", got)
		}
		if bps != 0 {
			t.Fatalf("freedompay = %d, want 0", bps)
		}
	})

	t.Run("rates are read per provider", func(t *testing.T) {
		t.Setenv("PAYMENTS_REFUND_ACQUIRING_BPS_TIPTOPPAY", " 290 ")

		got, err := refundAcquiringByProvider()
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		if got["tiptoppay"] != 290 {
			t.Fatalf("tiptoppay = %d, want 290", got["tiptoppay"])
		}
		if _, ok := got["partnerspay"]; ok {
			t.Fatalf("partnerspay must stay absent, got %v", got)
		}
	})

	// A set-but-unusable money knob must stop the boot instead of silently
	// falling back — "2.9" meaning 2.9% would otherwise hand the acquirer's
	// cost to the platform, invisibly.
	for name, value := range map[string]string{
		"percent instead of basis points": "2.9",
		"not a number":                    "none",
		"negative":                        "-1",
		"above 100%":                      "29000",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Setenv("PAYMENTS_REFUND_ACQUIRING_BPS_FREEDOMPAY", value)

			if _, err := refundAcquiringByProvider(); err == nil {
				t.Fatalf("value %q accepted, want an error", value)
			}
		})
	}
}

// knownPaymentProviders is a hand-kept mirror of the domain constants (the
// config layer deliberately does not import the domain). Nothing enforces the
// mirror at compile time, so a name that the domain would reject — a typo that
// silently disables that provider's knob — is caught here instead.
func TestKnownPaymentProvidersAreRealProviders(t *testing.T) {
	for _, name := range knownPaymentProviders {
		if p := domain.PaymentProvider(name); !p.Valid() {
			t.Errorf("knownPaymentProviders contains %q, which domain.PaymentProvider rejects", name)
		}
	}
}
