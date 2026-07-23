package domain

import (
	"errors"
	"testing"
)

func TestValidatePaymentTransition(t *testing.T) {
	cases := []struct {
		name string
		from PaymentStatus
		to   PaymentStatus
		want error
	}{
		{"created → authorized", PaymentCreated, PaymentAuthorized, nil},
		{"created → failed", PaymentCreated, PaymentFailed, nil},
		{"created → expired (checkout abandoned)", PaymentCreated, PaymentExpired, nil},
		{"authorized → captured", PaymentAuthorized, PaymentCaptured, nil},
		{"authorized → voided", PaymentAuthorized, PaymentVoided, nil},
		{"authorized → expired (hold lapsed)", PaymentAuthorized, PaymentExpired, nil},
		{"authorized → failed (capture declined)", PaymentAuthorized, PaymentFailed, nil},
		{"captured → partially_refunded", PaymentCaptured, PaymentPartiallyRefunded, nil},
		{"captured → refunded", PaymentCaptured, PaymentRefunded, nil},
		{"partially_refunded → refunded", PaymentPartiallyRefunded, PaymentRefunded, nil},

		// Money that was never taken cannot be given back.
		{"created → captured skips the hold", PaymentCreated, PaymentCaptured, ErrInvalidStatus},
		{"created → voided", PaymentCreated, PaymentVoided, ErrInvalidStatus},
		{"created → refunded", PaymentCreated, PaymentRefunded, ErrInvalidStatus},
		{"authorized → refunded", PaymentAuthorized, PaymentRefunded, ErrInvalidStatus},
		{"authorized → partially_refunded", PaymentAuthorized, PaymentPartiallyRefunded, ErrInvalidStatus},
		{"voided → captured", PaymentVoided, PaymentCaptured, ErrInvalidStatus},
		{"expired → captured", PaymentExpired, PaymentCaptured, ErrInvalidStatus},
		{"failed → authorized (retry is a NEW payment)", PaymentFailed, PaymentAuthorized, ErrInvalidStatus},
		// Refunded money cannot be returned twice, nor un-refunded.
		{"refunded → captured", PaymentRefunded, PaymentCaptured, ErrInvalidStatus},
		{"refunded → partially_refunded", PaymentRefunded, PaymentPartiallyRefunded, ErrInvalidStatus},
		{"captured → voided", PaymentCaptured, PaymentVoided, ErrInvalidStatus},
		{"captured → authorized (no way back)", PaymentCaptured, PaymentAuthorized, ErrInvalidStatus},
		{"partially_refunded → captured", PaymentPartiallyRefunded, PaymentCaptured, ErrInvalidStatus},
		// A second partial refund is not a status change at all.
		{"partially_refunded → partially_refunded", PaymentPartiallyRefunded, PaymentPartiallyRefunded, ErrInvalidStatus},
		{"same status is not a transition", PaymentAuthorized, PaymentAuthorized, ErrInvalidStatus},

		{"unknown source", PaymentStatus("pending"), PaymentCaptured, ErrValidation},
		{"unknown target", PaymentCreated, PaymentStatus("paid"), ErrValidation},
		{"empty statuses", PaymentStatus(""), PaymentStatus(""), ErrValidation},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePaymentTransition(tc.from, tc.to)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidatePaymentTransition(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
			}
			if want := tc.want == nil; CanPaymentTransition(tc.from, tc.to) != want {
				t.Fatalf("CanPaymentTransition(%q, %q) = %v, want %v", tc.from, tc.to, !want, want)
			}
		})
	}
}

func TestPaymentStatusPredicates(t *testing.T) {
	cases := []struct {
		status     PaymentStatus
		valid      bool
		terminal   bool
		holdsMoney bool
		refundable bool
	}{
		{PaymentCreated, true, false, false, false},
		{PaymentAuthorized, true, false, true, false},
		{PaymentCaptured, true, false, true, true},
		{PaymentPartiallyRefunded, true, false, false, true},
		{PaymentRefunded, true, true, false, false},
		{PaymentVoided, true, true, false, false},
		{PaymentFailed, true, true, false, false},
		{PaymentExpired, true, true, false, false},
		{PaymentStatus("paid"), false, true, false, false},
		{PaymentStatus(""), false, true, false, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.status.Terminal(); got != tc.terminal {
				t.Errorf("Terminal() = %v, want %v", got, tc.terminal)
			}
			if got := tc.status.HoldsMoney(); got != tc.holdsMoney {
				t.Errorf("HoldsMoney() = %v, want %v", got, tc.holdsMoney)
			}
			if got := tc.status.Refundable(); got != tc.refundable {
				t.Errorf("Refundable() = %v, want %v", got, tc.refundable)
			}
		})
	}
}

// HoldsMoney must mirror the partial unique index idx_payments_live_per_booking
// in migrations/0007_payments.sql: exactly the statuses listed there.
func TestHoldsMoneyMirrorsLiveIndex(t *testing.T) {
	live := map[PaymentStatus]bool{PaymentAuthorized: true, PaymentCapturing: true, PaymentCaptured: true}
	for status := range paymentTransitions {
		if got := status.HoldsMoney(); got != live[status] {
			t.Errorf("%q.HoldsMoney() = %v, index says %v", status, got, live[status])
		}
	}
}

func TestPaymentPurposeAndProviderValid(t *testing.T) {
	for purpose, want := range map[PaymentPurpose]bool{
		PurposeDeposit: true, PurposePreorder: true, "tip": false, "": false,
	} {
		if got := purpose.Valid(); got != want {
			t.Errorf("PaymentPurpose(%q).Valid() = %v, want %v", purpose, got, want)
		}
	}
	for provider, want := range map[PaymentProvider]bool{
		ProviderFreedomPay: true, ProviderTipTopPay: true, "FreedomPay": false, "stripe": false, "": false,
	} {
		if got := provider.Valid(); got != want {
			t.Errorf("PaymentProvider(%q).Valid() = %v, want %v", provider, got, want)
		}
	}
}
