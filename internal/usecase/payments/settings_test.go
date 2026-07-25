package payments

import (
	"testing"

	"backend-core/internal/domain"
)

// The per-provider rate is the one the settlement must use: a provider that
// keeps its fee on a reversal and one that returns it cannot share a number.
func TestRefundAcquiringBpsFor_ProviderOverrideWins(t *testing.T) {
	cfg := Config{
		RefundAcquiringBps: 0,
		RefundAcquiringBpsByProvider: map[domain.PaymentProvider]int{
			domain.ProviderTipTopPay: 290,
		},
	}.withDefaults()

	if got := cfg.refundAcquiringBpsFor(domain.ProviderTipTopPay); got != 290 {
		t.Fatalf("tiptoppay = %d, want 290 (its own rate)", got)
	}
	// FreedomPay was never configured — it falls back to the global rate.
	if got := cfg.refundAcquiringBpsFor(domain.ProviderFreedomPay); got != 0 {
		t.Fatalf("freedompay = %d, want 0 (global fallback)", got)
	}
}

// An explicit per-provider 0 must beat a non-zero global rate: "this acquirer
// charges nothing to reverse" is a real configuration, not an unset field.
func TestRefundAcquiringBpsFor_ProviderZeroBeatsGlobal(t *testing.T) {
	cfg := Config{
		RefundAcquiringBps: 100,
		RefundAcquiringBpsByProvider: map[domain.PaymentProvider]int{
			domain.ProviderFreedomPay: 0,
		},
	}.withDefaults()

	if got := cfg.refundAcquiringBpsFor(domain.ProviderFreedomPay); got != 0 {
		t.Fatalf("freedompay = %d, want 0 (explicit per-provider zero)", got)
	}
	if got := cfg.refundAcquiringBpsFor(domain.ProviderPartnersPay); got != 100 {
		t.Fatalf("partnerspay = %d, want 100 (global)", got)
	}
}

// A negative per-provider rate is dropped, not clamped — the provider then
// behaves exactly like one that was never configured.
func TestWithDefaults_NegativeProviderRateDropped(t *testing.T) {
	in := map[domain.PaymentProvider]int{domain.ProviderTipTopPay: -5}
	cfg := Config{RefundAcquiringBps: 100, RefundAcquiringBpsByProvider: in}.withDefaults()

	if got := cfg.refundAcquiringBpsFor(domain.ProviderTipTopPay); got != 100 {
		t.Fatalf("tiptoppay = %d, want 100 (negative override dropped)", got)
	}
	// withDefaults must not mutate the caller's map.
	if len(in) != 1 || in[domain.ProviderTipTopPay] != -5 {
		t.Fatalf("caller map mutated: %v", in)
	}
}

// An explicit "withhold nothing from the guest" (0 bps) is the owner-confirmed
// production setting, so withDefaults must NOT treat it as an unset field and
// silently restore a non-zero fallback — that would quietly take money out of
// every refund.
func TestWithDefaults_ExplicitZeroRefundAcquiringSurvives(t *testing.T) {
	got := Config{RefundAcquiringBps: 0}.withDefaults()
	if got.RefundAcquiringBps != 0 {
		t.Fatalf("RefundAcquiringBps = %d, want 0 (explicit zero must survive)", got.RefundAcquiringBps)
	}
}

// A negative value is nonsense (it would pay the guest MORE than was charged),
// so it falls back to the package default.
func TestWithDefaults_NegativeRefundAcquiringFallsBack(t *testing.T) {
	got := Config{RefundAcquiringBps: -1}.withDefaults()
	if got.RefundAcquiringBps != defaultRefundAcquiringBps {
		t.Fatalf("RefundAcquiringBps = %d, want %d", got.RefundAcquiringBps, defaultRefundAcquiringBps)
	}
}

// A configured non-zero rate is still honoured — the knob stays usable for the
// day the policy changes.
func TestWithDefaults_ConfiguredRefundAcquiringHonoured(t *testing.T) {
	got := Config{RefundAcquiringBps: 290}.withDefaults()
	if got.RefundAcquiringBps != 290 {
		t.Fatalf("RefundAcquiringBps = %d, want 290", got.RefundAcquiringBps)
	}
}
