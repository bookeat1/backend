package domain

import (
	"fmt"
	"math"
)

// Currency is an ISO-4217 alphabetic code, stored as VARCHAR(3).
type Currency string

// CurrencyKZT is the only currency the platform settles in today. It is not
// hard-coded into Money on purpose: the amount and the currency always travel
// together, so a second currency is a data change, not a refactor.
const CurrencyKZT Currency = "KZT"

// Valid reports whether c looks like an ISO-4217 code we accept.
func (c Currency) Valid() bool { return c == CurrencyKZT }

// Money-specific sentinels. Both wrap ErrValidation so the transport layer maps
// them to 422 without knowing anything about money.
var (
	// ErrCurrencyMismatch is returned when two amounts in different currencies
	// are combined. Adding KZT to USD is a bug, never a rounding question.
	ErrCurrencyMismatch = fmt.Errorf("currency mismatch: %w", ErrValidation)
	// ErrMoneyOverflow is returned when an operation would exceed int64. It
	// exists so an overflow surfaces as a rejected request rather than as a
	// silently negative amount.
	ErrMoneyOverflow = fmt.Errorf("money overflow: %w", ErrValidation)
	// ErrNegativeAmount is returned when an operation would produce a negative
	// amount where the domain forbids one (a payment, a fee, a refund).
	ErrNegativeAmount = fmt.Errorf("negative amount: %w", ErrValidation)
)

// Money is an exact amount in the minor unit of its currency (tiyn for KZT)
// plus the currency itself. There is deliberately no float anywhere near it:
// 0.1 + 0.2 is not 0.3, and in a payment domain that is someone's money.
//
// Money is a value type — every operation returns a new Money and never mutates
// the receiver.
type Money struct {
	AmountMinor int64
	Currency    Currency
}

// NewMoney builds a Money. It rejects unknown currencies and negative amounts:
// every amount this domain stores (payment, fee, refund, ledger entry) is
// non-negative, and direction is expressed by the ledger, not by a sign.
func NewMoney(amountMinor int64, currency Currency) (Money, error) {
	if !currency.Valid() {
		return Money{}, fmt.Errorf("unsupported currency %q: %w", currency, ErrValidation)
	}
	if amountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	return Money{AmountMinor: amountMinor, Currency: currency}, nil
}

// KZT is a shorthand for an amount in tiyn. It skips validation and is meant for
// literals in code and tests, not for values coming from the outside.
func KZT(amountMinor int64) Money {
	return Money{AmountMinor: amountMinor, Currency: CurrencyKZT}
}

// IsZero reports whether the amount is zero (regardless of currency).
func (m Money) IsZero() bool { return m.AmountMinor == 0 }

// Add returns m + other, or ErrCurrencyMismatch / ErrMoneyOverflow.
func (m Money) Add(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if other.AmountMinor > 0 && m.AmountMinor > math.MaxInt64-other.AmountMinor {
		return Money{}, ErrMoneyOverflow
	}
	if other.AmountMinor < 0 && m.AmountMinor < math.MinInt64-other.AmountMinor {
		return Money{}, ErrMoneyOverflow
	}
	return Money{AmountMinor: m.AmountMinor + other.AmountMinor, Currency: m.Currency}, nil
}

// Sub returns m − other. A result below zero is an error, not a negative
// balance: subtracting more than is left (refunding above the remainder) is a
// business rule violation and must be caught here rather than at the acquirer.
func (m Money) Sub(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if m.AmountMinor < other.AmountMinor {
		return Money{}, ErrNegativeAmount
	}
	return Money{AmountMinor: m.AmountMinor - other.AmountMinor, Currency: m.Currency}, nil
}

// Cmp returns -1, 0 or +1 comparing m to other. Currencies must match; the
// caller is expected to have validated that (Add/Sub report the mismatch), so
// Cmp on mismatched currencies is defined as 0 only for equal amounts and is
// never used as a money decision on its own.
func (m Money) Cmp(other Money) int {
	switch {
	case m.AmountMinor < other.AmountMinor:
		return -1
	case m.AmountMinor > other.AmountMinor:
		return 1
	default:
		return 0
	}
}

// String renders the amount in major units for logs and receipts, e.g.
// "9660.00 KZT". Never use it for arithmetic.
func (m Money) String() string {
	sign := ""
	a := m.AmountMinor
	if a < 0 {
		sign, a = "-", -a
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, a/100, a%100, m.Currency)
}

// BasisPointsDenominator is the scale of a basis point: 10000 bps = 100%.
// The whole domain expresses percentages in basis points because 3.5% as a
// float is a rounding error in somebody else's wallet (spec §4).
const BasisPointsDenominator int64 = 10000

// ApplyBasisPoints returns amount × bps / 10000, ROUNDED UP (ceiling).
//
// Rounding rule, deliberate and applied everywhere in this domain: fractions of
// a tiyn are always resolved in favour of the platform / the acquirer, never in
// favour of whoever would otherwise pay a fraction less. Reasons:
//
//   - it is deterministic and direction-stable — the same input always yields
//     the same output, and the platform can never end up collecting less than
//     the configured rate;
//   - the maximum distortion is one tiyn (0.01 ₸) per operation, which is below
//     the smallest unit any party can actually settle;
//   - the guest sees the exact total BEFORE paying (spec §3), so a rounded-up
//     fee is disclosed, not discovered.
//
// bps must be in [0, 10000]; anything else is ErrValidation.
func ApplyBasisPoints(amount Money, bps int) (Money, error) {
	if bps < 0 || int64(bps) > BasisPointsDenominator {
		return Money{}, fmt.Errorf("basis points %d out of range [0,%d]: %w", bps, BasisPointsDenominator, ErrValidation)
	}
	if amount.AmountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	if bps == 0 || amount.AmountMinor == 0 {
		return Money{AmountMinor: 0, Currency: amount.Currency}, nil
	}
	// Overflow guard before the multiplication: amount × bps must fit in int64.
	if amount.AmountMinor > math.MaxInt64/int64(bps) {
		return Money{}, ErrMoneyOverflow
	}
	product := amount.AmountMinor * int64(bps)
	// Ceiling division for non-negative operands.
	result := (product + BasisPointsDenominator - 1) / BasisPointsDenominator
	return Money{AmountMinor: result, Currency: amount.Currency}, nil
}

// ServiceFee returns the BookEat service fee charged on top of base at the given
// rate in basis points. It is a fee for the SERVICE (online booking, pre-order,
// a guaranteed table), never for the payment method — the wording "acquiring" /
// "card fee" is forbidden towards the guest (spec §3, §9.4).
//
// Rounded up, see ApplyBasisPoints.
func ServiceFee(base Money, bps int) (Money, error) {
	return ApplyBasisPoints(base, bps)
}

// TotalWithFee returns (fee, total) for a base amount at the given rate:
// total = base + fee. This is a plain additive markup: the fee is bps of the
// BASE. Kept as a domain primitive; the payment flow uses GrossUpForAcquirer
// instead (see its doc for why additive is the wrong tool for covering an
// acquirer fee).
func TotalWithFee(base Money, bps int) (fee Money, total Money, err error) {
	fee, err = ServiceFee(base, bps)
	if err != nil {
		return Money{}, Money{}, err
	}
	total, err = base.Add(fee)
	if err != nil {
		return Money{}, Money{}, err
	}
	return fee, total, nil
}

// GrossUpForAcquirer returns (fee, total) such that after the acquirer withholds
// acquirerBps of the TOTAL, the venue still nets the full base.
//
// This is NOT base + base×bps. The acquirer takes its cut from the total charged
// to the guest, not from the base, so a plain additive markup always leaves the
// venue short by bps of the fee itself. The correct amount grosses up:
//
//	total = ceil( base × 10000 / (10000 − acquirerBps) )
//	fee   = total − base
//
// Because total is rounded UP, total×(10000−acquirerBps) ≥ base×10000, i.e. the
// net after the acquirer's cut is always ≥ base — the rounding remainder (≤ 1
// tiyn) falls in the VENUE's favour, never the guest's shortfall. Under the
// subscription model BookEat's own take on the payment is ~zero: the fee exists
// only to make the venue whole against the acquirer (spec §9.2). The guest-facing
// label stays "service fee", never "card/acquiring fee" (spec §3, §9.4).
//
// acquirerBps must be in [0, 10000). 10000 (100%) is rejected: it would mean the
// acquirer takes everything, leaving no finite total that nets a positive base.
func GrossUpForAcquirer(base Money, acquirerBps int) (fee Money, total Money, err error) {
	if acquirerBps < 0 || int64(acquirerBps) >= BasisPointsDenominator {
		return Money{}, Money{}, fmt.Errorf("acquirer basis points %d out of range [0,%d): %w", acquirerBps, BasisPointsDenominator, ErrValidation)
	}
	if base.AmountMinor < 0 {
		return Money{}, Money{}, ErrNegativeAmount
	}
	if acquirerBps == 0 || base.AmountMinor == 0 {
		return Money{AmountMinor: 0, Currency: base.Currency},
			Money{AmountMinor: base.AmountMinor, Currency: base.Currency}, nil
	}
	// Overflow guard before the multiplication: base × 10000 must fit in int64.
	if base.AmountMinor > math.MaxInt64/BasisPointsDenominator {
		return Money{}, Money{}, ErrMoneyOverflow
	}
	denom := BasisPointsDenominator - int64(acquirerBps)
	numerator := base.AmountMinor * BasisPointsDenominator
	// Ceiling division for non-negative operands: round the total UP so the net
	// after the acquirer's cut never falls below base.
	totalMinor := (numerator + denom - 1) / denom
	total = Money{AmountMinor: totalMinor, Currency: base.Currency}
	fee, err = total.Sub(base)
	if err != nil {
		return Money{}, Money{}, err
	}
	return fee, total, nil
}
