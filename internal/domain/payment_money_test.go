package domain

import (
	"errors"
	"math"
	"testing"
)

func TestMoneyAdd(t *testing.T) {
	cases := []struct {
		name    string
		a, b    Money
		want    int64
		wantErr error
	}{
		{"plain", KZT(9660), KZT(340), 10000, nil},
		{"zero", KZT(0), KZT(0), 0, nil},
		{"currency mismatch", KZT(100), Money{100, "USD"}, 0, ErrCurrencyMismatch},
		{"overflow", KZT(math.MaxInt64), KZT(1), 0, ErrMoneyOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.a.Add(tc.b)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Add() error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && got.AmountMinor != tc.want {
				t.Fatalf("Add() = %d, want %d", got.AmountMinor, tc.want)
			}
		})
	}
}

func TestMoneySub(t *testing.T) {
	cases := []struct {
		name    string
		a, b    Money
		want    int64
		wantErr error
	}{
		{"plain", KZT(10000), KZT(100), 9900, nil},
		{"to zero", KZT(10000), KZT(10000), 0, nil},
		// Refunding more than what is left must fail here, not at the acquirer.
		{"below zero", KZT(100), KZT(101), 0, ErrNegativeAmount},
		{"currency mismatch", KZT(100), Money{1, "USD"}, 0, ErrCurrencyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.a.Sub(tc.b)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Sub() error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && got.AmountMinor != tc.want {
				t.Fatalf("Sub() = %d, want %d", got.AmountMinor, tc.want)
			}
		})
	}
}

func TestNewMoney(t *testing.T) {
	if _, err := NewMoney(-1, CurrencyKZT); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("NewMoney(-1) error = %v, want ErrNegativeAmount", err)
	}
	if _, err := NewMoney(1, "USD"); !errors.Is(err, ErrValidation) {
		t.Errorf("NewMoney(USD) error = %v, want ErrValidation", err)
	}
	m, err := NewMoney(0, CurrencyKZT)
	if err != nil || !m.IsZero() {
		t.Errorf("NewMoney(0) = %v, %v; want zero money, nil", m, err)
	}
}

func TestMoneyStringAndCmp(t *testing.T) {
	if got := KZT(966000).String(); got != "9660.00 KZT" {
		t.Errorf("String() = %q, want %q", got, "9660.00 KZT")
	}
	if got := KZT(5).String(); got != "0.05 KZT" {
		t.Errorf("String() = %q, want %q", got, "0.05 KZT")
	}
	if got := (Money{-105, CurrencyKZT}).String(); got != "-1.05 KZT" {
		t.Errorf("String() = %q, want %q", got, "-1.05 KZT")
	}
	if KZT(1).Cmp(KZT(2)) != -1 || KZT(2).Cmp(KZT(1)) != 1 || KZT(2).Cmp(KZT(2)) != 0 {
		t.Error("Cmp() does not order amounts")
	}
}

func TestServiceFeeRoundsUp(t *testing.T) {
	cases := []struct {
		name string
		base int64
		bps  int
		want int64
	}{
		// The spec example: 9 660 tiyn base at 3.5% → 338.1, rounded up.
		{"spec example 3.5%", 9660, 350, 339},
		{"exact division", 10000, 350, 350},
		{"1% of a round sum", 10000, 100, 100},
		// Anything above an exact multiple costs one more tiyn — never less.
		{"one tiyn over", 10001, 100, 101},
		{"one tiyn under", 9999, 100, 100},
		{"smallest non-zero result", 1, 1, 1},
		{"zero base", 0, 350, 0},
		{"zero rate", 1_000_000, 0, 0},
		{"full 100%", 12345, 10000, 12345},
		{"large amount, no overflow", 1_000_000_000_000, 350, 35_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ServiceFee(KZT(tc.base), tc.bps)
			if err != nil {
				t.Fatalf("ServiceFee() error = %v", err)
			}
			if got.AmountMinor != tc.want {
				t.Fatalf("ServiceFee(%d, %dbps) = %d, want %d", tc.base, tc.bps, got.AmountMinor, tc.want)
			}
			if got.Currency != CurrencyKZT {
				t.Fatalf("ServiceFee() currency = %q, want KZT", got.Currency)
			}
		})
	}
}

// The rounding rule is a business decision, not an accident: the platform never
// collects less than the configured rate, and the guest never sees a number
// they were not shown before paying. Property: fee ≥ exact rate, and the
// difference is under one tiyn.
func TestServiceFeeNeverRoundsDown(t *testing.T) {
	for base := int64(0); base < 500; base++ {
		for _, bps := range []int{1, 100, 350, 999, 10000} {
			fee, err := ServiceFee(KZT(base), bps)
			if err != nil {
				t.Fatalf("ServiceFee(%d, %d) error = %v", base, bps, err)
			}
			exactNumerator := base * int64(bps)
			if fee.AmountMinor*BasisPointsDenominator < exactNumerator {
				t.Fatalf("ServiceFee(%d, %dbps) = %d rounds DOWN", base, bps, fee.AmountMinor)
			}
			if (fee.AmountMinor-1)*BasisPointsDenominator >= exactNumerator && fee.AmountMinor > 0 {
				t.Fatalf("ServiceFee(%d, %dbps) = %d overshoots by a full tiyn", base, bps, fee.AmountMinor)
			}
		}
	}
}

func TestApplyBasisPointsErrors(t *testing.T) {
	if _, err := ApplyBasisPoints(KZT(100), -1); !errors.Is(err, ErrValidation) {
		t.Errorf("negative bps error = %v, want ErrValidation", err)
	}
	if _, err := ApplyBasisPoints(KZT(100), 10001); !errors.Is(err, ErrValidation) {
		t.Errorf("bps > 100%% error = %v, want ErrValidation", err)
	}
	if _, err := ApplyBasisPoints(Money{-1, CurrencyKZT}, 350); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("negative amount error = %v, want ErrNegativeAmount", err)
	}
	if _, err := ApplyBasisPoints(KZT(math.MaxInt64), 350); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("overflow error = %v, want ErrMoneyOverflow", err)
	}
}

func TestTotalWithFee(t *testing.T) {
	fee, total, err := TotalWithFee(KZT(9660), 350)
	if err != nil {
		t.Fatalf("TotalWithFee() error = %v", err)
	}
	if fee.AmountMinor != 339 || total.AmountMinor != 9999 {
		t.Fatalf("TotalWithFee() = fee %d, total %d; want 339, 9999", fee.AmountMinor, total.AmountMinor)
	}
	if _, _, err := TotalWithFee(KZT(math.MaxInt64-1), 10000); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("TotalWithFee() overflow error = %v, want ErrMoneyOverflow", err)
	}
}
