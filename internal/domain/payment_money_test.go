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

func TestGrossUpForAcquirer_VenueMadeWhole(t *testing.T) {
	// For a range of bases and acquirer rates, the venue must always net at
	// least the base after the acquirer withholds its cut of the total, and the
	// shortfall-in-its-favour (dust) must never exceed one tiyn.
	rates := []int{0, 100, 290, 350, 500, 1000, 9900}
	bases := []int64{0, 1, 99, 100, 12_345, 1_000_000, 999_999_999}
	for _, bps := range rates {
		for _, baseMinor := range bases {
			base := KZT(baseMinor)
			fee, total, err := GrossUpForAcquirer(base, bps)
			if err != nil {
				t.Fatalf("GrossUpForAcquirer(%d, %d) error = %v", baseMinor, bps, err)
			}
			if total.AmountMinor != base.AmountMinor+fee.AmountMinor {
				t.Fatalf("bps=%d base=%d: total %d != base+fee %d", bps, baseMinor, total.AmountMinor, base.AmountMinor+fee.AmountMinor)
			}
			// Net after the acquirer's cut of the TOTAL (acquirer floors its fee,
			// which only helps the venue; we model the worst case with a ceil cut).
			acquirerCut := (total.AmountMinor*int64(bps) + BasisPointsDenominator - 1) / BasisPointsDenominator
			net := total.AmountMinor - acquirerCut
			if net < base.AmountMinor {
				t.Fatalf("bps=%d base=%d: net to venue %d < base %d", bps, baseMinor, net, base.AmountMinor)
			}
			if net-base.AmountMinor > 1 {
				t.Fatalf("bps=%d base=%d: dust %d exceeds 1 tiyn (net %d, base %d)", bps, baseMinor, net-base.AmountMinor, net, base.AmountMinor)
			}
		}
	}
}

func TestGrossUpForAcquirer_KnownValues(t *testing.T) {
	// 3.5% acquirer on 10,000.00 ₸ (1,000,000 tiyn): total = ceil(1e6*1e4/9650)
	// = 1,036,270; fee = 36,270. A plain additive 3.5% (35,000) would be short.
	fee, total, err := GrossUpForAcquirer(KZT(1_000_000), 350)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if total.AmountMinor != 1_036_270 || fee.AmountMinor != 36_270 {
		t.Fatalf("got total=%d fee=%d, want total=1036270 fee=36270", total.AmountMinor, fee.AmountMinor)
	}
	// Zero rate → no markup.
	fee, total, err = GrossUpForAcquirer(KZT(500), 0)
	if err != nil || fee.AmountMinor != 0 || total.AmountMinor != 500 {
		t.Fatalf("zero-rate: got total=%d fee=%d err=%v, want 500/0/nil", total.AmountMinor, fee.AmountMinor, err)
	}
}

func TestGrossUpForAcquirer_Errors(t *testing.T) {
	if _, _, err := GrossUpForAcquirer(KZT(100), 10000); !errors.Is(err, ErrValidation) {
		t.Errorf("bps=10000 (100%%) error = %v, want ErrValidation", err)
	}
	if _, _, err := GrossUpForAcquirer(KZT(100), -1); !errors.Is(err, ErrValidation) {
		t.Errorf("bps=-1 error = %v, want ErrValidation", err)
	}
	if _, _, err := GrossUpForAcquirer(Money{AmountMinor: -5, Currency: "KZT"}, 350); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("negative base error = %v, want ErrNegativeAmount", err)
	}
	if _, _, err := GrossUpForAcquirer(KZT(math.MaxInt64-1), 350); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("overflow error = %v, want ErrMoneyOverflow", err)
	}
}
