package domain

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// Payout fee — what it costs US to move money TO a venue
//
// Moving money out is not free. FreedomPay's merchant questionnaire (tariff of
// 14.07.2026) prices a payout to a KZ bank card at 1.9% of the payout with a
// MINIMUM of 300 ₸ per payout. The floor is the whole reason payouts are
// batched daily rather than made per booking: at 300 ₸ minimum, a 10 000 ₸
// payout costs 3%, a 1 000 ₸ payout costs 30%, and a 100 ₸ payout costs more
// than it delivers.
//
// This file models that cost explicitly so it is a number in the system, not a
// surprise on the acquirer's statement.
// ---------------------------------------------------------------------------

// PayoutFeeBearer says WHO ends up paying the acquirer's payout fee. It is a
// business policy, configurable per deployment, and it is stored on every
// payout so a historical payout is still readable after the policy changes.
type PayoutFeeBearer string

const (
	// PayoutFeeBearerPlatform: BookEat absorbs the fee. The venue receives
	// exactly the amount its statement showed as owed. This is the DEFAULT and
	// the safe one — it can never surprise a venue with a payout smaller than
	// the statement it already read.
	PayoutFeeBearerPlatform PayoutFeeBearer = "platform"
	// PayoutFeeBearerVenue: the fee is deducted from the venue's payout, so the
	// venue receives gross − fee. Cheaper for BookEat, but it makes the amount
	// that lands on the venue's card differ from the amount its statement calls
	// "owed" — a support conversation waiting to happen, which is why it is not
	// the default and must be turned on deliberately.
	PayoutFeeBearerVenue PayoutFeeBearer = "venue"
)

// Valid reports whether b is a known bearer.
func (b PayoutFeeBearer) Valid() bool {
	return b == PayoutFeeBearerPlatform || b == PayoutFeeBearerVenue
}

// PayoutFee returns the acquirer's cost of ONE payout of gross: a rate in basis
// points on the gross amount, raised to a hard MINIMUM floor.
//
//	fee = max( ceil(gross × bps / 10000), minimumMinor )
//
// The rate part rounds UP through ApplyBasisPoints — the same direction the
// whole money domain rounds, in favour of whoever collects the fee, so we never
// under-estimate what the acquirer will actually take. The floor is applied
// AFTER the rate, never blended into it: the acquirer charges max(rate, floor)
// per payout, and the break-even where the rate finally exceeds the floor is
// minimumMinor / (bps/10000) — with the FreedomPay defaults (190 bps, 300 ₸)
// that is 15 789.47 ₸, i.e. every payout below ~15 800 ₸ costs a flat 300 ₸.
// That number is what the daily minimum-payout threshold exists to manage.
//
// A ZERO gross costs nothing: the floor is a per-payout charge, and a payout of
// nothing is never dispatched, so charging a floor on it would invent a cost.
//
// bps must be in [0, 10000]; minimumMinor must be non-negative.
func PayoutFee(gross Money, bps int, minimumMinor int64) (Money, error) {
	if minimumMinor < 0 {
		return Money{}, fmt.Errorf("payout fee minimum %d must be non-negative: %w", minimumMinor, ErrValidation)
	}
	if gross.AmountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	if gross.AmountMinor == 0 {
		return Money{AmountMinor: 0, Currency: gross.Currency}, nil
	}
	fee, err := ApplyBasisPoints(gross, bps)
	if err != nil {
		return Money{}, err
	}
	if fee.AmountMinor < minimumMinor {
		fee.AmountMinor = minimumMinor
	}
	return fee, nil
}

// NetPayoutAmount returns what actually leaves for the venue's card, given the
// gross owed, the computed fee and who bears it.
//
//   - platform bears it → the venue gets the full gross (BookEat pays gross+fee
//     in total);
//   - venue bears it → the venue gets gross − fee, and a fee that would eat the
//     whole payout is rejected as ErrNegativeAmount rather than dispatched as a
//     zero or negative transfer.
func NetPayoutAmount(gross, fee Money, bearer PayoutFeeBearer) (Money, error) {
	if gross.Currency != fee.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if bearer != PayoutFeeBearerVenue {
		return gross, nil
	}
	net, err := gross.Sub(fee)
	if err != nil {
		return Money{}, err
	}
	if net.AmountMinor <= 0 {
		return Money{}, fmt.Errorf("payout fee %s consumes the whole payout %s: %w", fee, gross, ErrNegativeAmount)
	}
	return net, nil
}
