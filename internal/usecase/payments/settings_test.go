package payments

import "testing"

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
