package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPayoutFee_RateAndFloor walks the fee across the floor, which is the only
// interesting region: below the break-even the fee is a flat 300 ₸, above it
// the 1.9% rate takes over. Amounts are in tiyn (1 ₸ = 100 tiyn).
//
// MUTATION CHECK: removing the `fee < minimum -> minimum` clause in PayoutFee
// makes every case below the break-even fail.
func TestPayoutFee_RateAndFloor(t *testing.T) {
	const (
		bps = 190    // 1.9%
		min = 30_000 // 300 ₸
	)
	cases := []struct {
		name     string
		grossKZT int64 // in tiyn
		wantFee  int64
	}{
		{"far below the break-even: the floor is the whole cost", 50_000, 30_000},          // 500 ₸ -> 300 ₸ (60%!)
		{"the owner's example: 10 000 ₸ costs 300 ₸ = 3%", 1_000_000, 30_000},              // 1.9% = 190 ₸ < floor
		{"just below the break-even, still the floor", 1_578_000, 30_000},                  // 1.9% = 299.82 ₸
		{"AT the break-even, rate and floor meet exactly", 1_578_947, 30_000},              // 1.9% = 300.00 ₸ (ceil)
		{"ONE tiyn above the break-even, the rate takes over", 1_578_948, 30_001},          // the floor stops binding
		{"comfortably above: pure rate", 1_600_000, 30_400},                                // 1.9% = 304 ₸
		{"well above: pure rate", 10_000_000, 190_000},                                     // 100 000 ₸ -> 1 900 ₸
		{"rounding is UP, never in the acquirer's disfavour", 1_600_001, 30_401},           // 30400.019 -> 30401
		{"zero gross costs nothing: a floor on nothing would invent a cost", 0, 0},         //
		{"one tiyn still pays the floor — this is why small payouts roll over", 1, 30_000}, //
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fee, err := PayoutFee(KZT(tc.grossKZT), bps, min)
			if err != nil {
				t.Fatalf("PayoutFee: %v", err)
			}
			if fee.AmountMinor != tc.wantFee {
				t.Fatalf("gross %d: expected fee %d, got %d", tc.grossKZT, tc.wantFee, fee.AmountMinor)
			}
			if fee.Currency != CurrencyKZT {
				t.Fatalf("fee lost its currency: %q", fee.Currency)
			}
		})
	}
}

// TestPayoutFee_RejectsNonsense keeps the guards honest.
func TestPayoutFee_RejectsNonsense(t *testing.T) {
	if _, err := PayoutFee(KZT(1000), 190, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("a negative minimum must be rejected, got %v", err)
	}
	if _, err := PayoutFee(KZT(1000), 10_001, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bps above 100%% must be rejected, got %v", err)
	}
	if _, err := PayoutFee(Money{AmountMinor: -5, Currency: CurrencyKZT}, 190, 0); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("a negative gross must be rejected, got %v", err)
	}
}

// TestNetPayoutAmount_ByBearer pins the WHO-PAYS policy, including the case the
// minimum-payout guard exists to prevent.
func TestNetPayoutAmount_ByBearer(t *testing.T) {
	gross, fee := KZT(1_000_000), KZT(30_000)

	net, err := NetPayoutAmount(gross, fee, PayoutFeeBearerPlatform)
	if err != nil {
		t.Fatalf("platform bearer: %v", err)
	}
	if net.AmountMinor != 1_000_000 {
		t.Fatalf("platform absorbs the fee: the venue must get the full gross, got %d", net.AmountMinor)
	}

	net, err = NetPayoutAmount(gross, fee, PayoutFeeBearerVenue)
	if err != nil {
		t.Fatalf("venue bearer: %v", err)
	}
	if net.AmountMinor != 970_000 {
		t.Fatalf("venue bears the fee: expected 970 000, got %d", net.AmountMinor)
	}

	// A payout the fee would swallow whole is refused rather than dispatched as
	// zero or negative. With the default minimum this is unreachable through
	// the daily pass; it is the last line of defence if that knob is lowered.
	if _, err := NetPayoutAmount(KZT(20_000), fee, PayoutFeeBearerVenue); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("a fee larger than the payout must be refused, got %v", err)
	}
}

// TestPayoutLedgerEntries_BalanceUnderBothPolicies is the accounting invariant:
// whichever side pays the fee, the batch balances and the acquirer is credited
// everything that actually leaves BookEat's account.
func TestPayoutLedgerEntries_BalanceUnderBothPolicies(t *testing.T) {
	base := Payout{ID: uuid.New(), Currency: CurrencyKZT, GrossAmountMinor: 1_000_000, FeeMinor: 30_000}

	platform := base
	platform.AmountMinor, platform.FeeBearer = 1_000_000, PayoutFeeBearerPlatform
	venue := base
	venue.AmountMinor, venue.FeeBearer = 970_000, PayoutFeeBearerVenue

	for _, p := range []Payout{platform, venue} {
		entries := PayoutLedgerEntries(p, time.Now())
		if err := ValidatePayoutLedgerBalance(entries); err != nil {
			t.Fatalf("%s policy: ledger does not balance: %v", p.FeeBearer, err)
		}
		var acquirer int64
		for _, e := range entries {
			if e.Account == AccountAcquirer && e.Direction == DirectionCredit {
				acquirer += e.AmountMinor
			}
		}
		want := p.AmountMinor + p.FeeMinor
		if acquirer != want {
			t.Fatalf("%s policy: acquirer credited %d, expected %d (transfer + fee)", p.FeeBearer, acquirer, want)
		}
	}
}
