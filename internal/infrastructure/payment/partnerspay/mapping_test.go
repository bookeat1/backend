package partnerspay

import (
	"testing"

	"backend-core/internal/domain"
)

// TestMapPaymentStatusNeverReadsUnknownAsPaid is the one property this
// scaffold's mapping.go must never lose, since there is nothing else to test
// yet (no real status word is known — see the TODO(contract) notes in
// mapping.go). An unrecognised, or empty, status must never be reported as
// PaymentCaptured or PaymentAuthorized: that is exactly the mistake that
// would make a booking look paid when we have no idea what Partners Pay
// actually meant.
func TestMapPaymentStatusNeverReadsUnknownAsPaid(t *testing.T) {
	for _, raw := range []string{"", "success", "SUCCESS", "paid", "completed", "captured", "ok", "1", "garbage"} {
		status, known := mapPaymentStatus(raw)
		if known {
			t.Errorf("mapPaymentStatus(%q) known = true, want false (no status word is confirmed yet)", raw)
		}
		if status == domain.PaymentCaptured || status == domain.PaymentAuthorized {
			t.Errorf("mapPaymentStatus(%q) = %q, must never read an unrecognised status as money moved", raw, status)
		}
	}
}

// TestMapRefundStatusNeverReadsUnknownAsSucceeded mirrors the same rule for
// refunds: an unrecognised status must not read as RefundSucceeded.
func TestMapRefundStatusNeverReadsUnknownAsSucceeded(t *testing.T) {
	for _, raw := range []string{"", "success", "refunded", "ok", "1", "garbage"} {
		status, known := mapRefundStatus(raw)
		if known {
			t.Errorf("mapRefundStatus(%q) known = true, want false (no status word is confirmed yet)", raw)
		}
		if status == domain.RefundSucceeded {
			t.Errorf("mapRefundStatus(%q) = %q, must never read an unrecognised status as refunded", raw, status)
		}
	}
}
