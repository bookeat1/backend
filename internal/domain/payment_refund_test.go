package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// capturedPayment is the spec §9.1 example in tiyn: 10 000 ₸ total = 9 660 ₸
// base + 340 ₸ service fee.
func capturedPayment() Payment {
	return Payment{
		ID:              uuid.New(),
		BookingID:       uuid.New(),
		Status:          PaymentCaptured,
		AmountMinor:     1_000_000,
		BaseAmountMinor: 966_000,
		FeeMinor:        34_000,
		Currency:        CurrencyKZT,
	}
}

// TestSettleRefund walks every row of the settlement table in spec §9.1
// (owner decision, variant A).
func TestSettleRefund(t *testing.T) {
	deadline := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	policy := RefundPolicy{AcquiringBps: 100} // 1%

	cases := []struct {
		name    string
		trigger RefundTrigger
		at      time.Time
		want    Settlement
	}{
		{
			name:    "guest cancels before the deadline: all back except 1% acquiring",
			trigger: RefundTriggerGuestCancel,
			at:      deadline.Add(-time.Minute),
			want:    Settlement{GuestMinor: 990_000, AcquirerMinor: 10_000, Currency: CurrencyKZT},
		},
		{
			name:    "guest cancels after the deadline: settled as a no-show",
			trigger: RefundTriggerGuestCancel,
			at:      deadline.Add(time.Minute),
			want:    Settlement{RestaurantMinor: 966_000, PlatformMinor: 34_000, Currency: CurrencyKZT},
		},
		{
			// The deadline itself is already "after": the venue's protection
			// starts at the moment it was promised, not a second later.
			name:    "guest cancels exactly at the deadline: late",
			trigger: RefundTriggerGuestCancel,
			at:      deadline,
			want:    Settlement{RestaurantMinor: 966_000, PlatformMinor: 34_000, Currency: CurrencyKZT},
		},
		{
			name:    "no-show: base to the venue, fee to the platform",
			trigger: RefundTriggerNoShow,
			at:      deadline.Add(-48 * time.Hour), // timing is irrelevant here
			want:    Settlement{RestaurantMinor: 966_000, PlatformMinor: 34_000, Currency: CurrencyKZT},
		},
		{
			name:    "venue cancels: full refund, service fee included",
			trigger: RefundTriggerVenueCancel,
			at:      deadline.Add(time.Minute),
			want:    Settlement{GuestMinor: 1_000_000, Currency: CurrencyKZT},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := capturedPayment()
			got, err := SettleRefund(p, tc.trigger, tc.at, deadline, policy)
			if err != nil {
				t.Fatalf("SettleRefund() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("SettleRefund() = %+v, want %+v", got, tc.want)
			}
			if got.Total().AmountMinor != p.AmountMinor {
				t.Fatalf("settlement total = %d, payment total = %d", got.Total().AmountMinor, p.AmountMinor)
			}
		})
	}
}

func TestSettleRefundRoundsAcquiringUp(t *testing.T) {
	deadline := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	p := capturedPayment()
	p.AmountMinor = 1_000_001 // one tiyn above a round sum
	p.BaseAmountMinor = 966_001

	got, err := SettleRefund(p, RefundTriggerGuestCancel, deadline.Add(-time.Hour), deadline, RefundPolicy{AcquiringBps: 100})
	if err != nil {
		t.Fatalf("SettleRefund() error = %v", err)
	}
	// 1% of 1 000 001 is 10 000.01 → withheld 10 001, never 10 000.
	if got.AcquirerMinor != 10_001 || got.GuestMinor != 990_000 {
		t.Fatalf("SettleRefund() = %+v, want acquirer 10001 / guest 990000", got)
	}
	if got.Total().AmountMinor != p.AmountMinor {
		t.Fatalf("settlement total = %d, want %d", got.Total().AmountMinor, p.AmountMinor)
	}
}

func TestSettleRefundRejectsBadInput(t *testing.T) {
	deadline := time.Now()
	policy := RefundPolicy{AcquiringBps: 100}

	t.Run("unknown trigger", func(t *testing.T) {
		if _, err := SettleRefund(capturedPayment(), RefundTrigger("chargeback"), deadline, deadline, policy); !errors.Is(err, ErrValidation) {
			t.Fatalf("error = %v, want ErrValidation", err)
		}
	})

	// A hold is voided, never settled: there is no money to split yet.
	for _, status := range []PaymentStatus{PaymentCreated, PaymentAuthorized, PaymentVoided, PaymentRefunded, PaymentExpired, PaymentFailed} {
		t.Run("status "+string(status), func(t *testing.T) {
			p := capturedPayment()
			p.Status = status
			if _, err := SettleRefund(p, RefundTriggerGuestCancel, deadline, deadline, policy); !errors.Is(err, ErrInvalidStatus) {
				t.Fatalf("error = %v, want ErrInvalidStatus", err)
			}
		})
	}

	t.Run("amount does not add up", func(t *testing.T) {
		p := capturedPayment()
		p.FeeMinor++
		if _, err := SettleRefund(p, RefundTriggerGuestCancel, deadline, deadline, policy); !errors.Is(err, ErrValidation) {
			t.Fatalf("error = %v, want ErrValidation", err)
		}
	})
}

func TestRefundStatusValid(t *testing.T) {
	for s, want := range map[RefundStatus]bool{
		RefundCreated: true, RefundSucceeded: true, RefundFailed: true, "pending": false, "": false,
	} {
		if got := s.Valid(); got != want {
			t.Errorf("RefundStatus(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}
