package domain

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func item(price int64, qty int, status BookingItemStatus) BookingItem {
	return BookingItem{ID: uuid.New(), PriceMinor: price, Quantity: qty, Status: status}
}

func TestSumPreorderItems(t *testing.T) {
	// Non-cancelled lines are summed; a cancelled line is excluded — the same
	// rule the payment charge (resolveAmount) and the guest total (buildPreorder)
	// share via this ONE helper.
	items := []BookingItem{
		item(450000, 2, BookingItemPending),   // 900000
		item(100000, 1, BookingItemConfirmed), // 100000
		item(999999, 3, BookingItemCancelled), // excluded
	}
	got, err := SumPreorderItems(items)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got != 1000000 {
		t.Errorf("sum = %d, want 1000000 (cancelled excluded)", got)
	}

	// Empty set → zero, no error.
	if got, err := SumPreorderItems(nil); err != nil || got != 0 {
		t.Errorf("sum(nil) = %d, %v; want 0, nil", got, err)
	}

	// Per-line overflow is surfaced as an error, never a wrapped negative.
	if _, err := SumPreorderItems([]BookingItem{item(math.MaxInt64, 2, BookingItemPending)}); !errors.Is(err, ErrValidation) {
		t.Errorf("per-line overflow err = %v, want ErrValidation", err)
	}

	// Accumulation overflow across two near-max lines is caught too.
	huge := []BookingItem{
		item(math.MaxInt64, 1, BookingItemPending),
		item(1, 1, BookingItemPending),
	}
	if _, err := SumPreorderItems(huge); !errors.Is(err, ErrValidation) {
		t.Errorf("accumulation overflow err = %v, want ErrValidation", err)
	}
}

func TestBookingItemTotalMinorChecked(t *testing.T) {
	if got, err := item(450000, 2, BookingItemPending).TotalMinorChecked(); err != nil || got != 900000 {
		t.Errorf("TotalMinorChecked = %d, %v; want 900000, nil", got, err)
	}
	if _, err := (item(math.MaxInt64, 2, BookingItemPending)).TotalMinorChecked(); !errors.Is(err, ErrValidation) {
		t.Errorf("overflow err = %v, want ErrValidation", err)
	}
	// TotalMinor (display) saturates to MaxInt64 on overflow, never wraps.
	if got := (item(math.MaxInt64, 2, BookingItemPending)).TotalMinor(); got != math.MaxInt64 {
		t.Errorf("TotalMinor on overflow = %d, want math.MaxInt64 (saturated, not wrapped)", got)
	}
}

// The pre-order freeze in usecase/preorder depends on NonTerminalPaymentStatuses
// including `created` (amount snapshotted, captured later by the webhook) and
// excluding the truly-done states. This pins that contract.
func TestNonTerminalPaymentStatuses(t *testing.T) {
	set := map[PaymentStatus]bool{}
	for _, s := range NonTerminalPaymentStatuses() {
		set[s] = true
	}
	// Must freeze the pre-order (money in play or about to be).
	for _, s := range []PaymentStatus{
		PaymentCreated, PaymentAuthorized, PaymentCapturing, PaymentVoiding,
		PaymentCaptured, PaymentPartiallyRefunded,
	} {
		if !set[s] {
			t.Errorf("status %q missing from NonTerminalPaymentStatuses (must freeze pre-order)", s)
		}
		if s.Terminal() {
			t.Errorf("status %q reports Terminal()=true but must be non-terminal", s)
		}
	}
	// Must NOT freeze (guest may re-order after these).
	for _, s := range []PaymentStatus{
		PaymentVoided, PaymentExpired, PaymentFailed, PaymentRefunded,
	} {
		if set[s] {
			t.Errorf("terminal status %q must not be in NonTerminalPaymentStatuses", s)
		}
		if !s.Terminal() {
			t.Errorf("status %q reports Terminal()=false but must be terminal", s)
		}
	}
}
