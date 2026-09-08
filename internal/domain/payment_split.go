package domain

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Split payments — one charge, several recipients
//
// A guest pays ONE amount. That amount is not ours: the venue's share is the
// base (the deposit / the pre-order / the ticket) and the platform keeps the
// service fee that was grossed up on top of it. Until now that division only
// existed AFTER the money had landed on our merchant account — as ledger lines
// (usecase/payments/ledger.go) settled later by a payout (usecase/payouts).
//
// A split payment expresses the same division at the acquirer, at the moment of
// the charge: the acquirer credits each recipient's own sub-merchant account
// directly. It is deliberately the SAME notion of commission, not a second one:
//
//	venue share    = Payment.BaseAmountMinor   (AccountRestaurant in the ledger)
//	platform share = Payment.FeeMinor          (AccountPlatform in the ledger)
//	                 -----------------------
//	                 = Payment.AmountMinor     (what the guest is charged)
//
// Because Payment.AmountMinor is defined as BaseAmountMinor + FeeMinor (and the
// database enforces it, chk_payments_amount_split), the shares add up to the
// total by construction — there is no second rounding step that could invent or
// lose a tiyn. Everything here is integer minor units; the decimal string the
// acquirer wants is produced at the adapter boundary only
// (infrastructure/payment.FormatMinor).
// ---------------------------------------------------------------------------

// SplitPayee names WHO a share belongs to, in domain terms. It is not an
// account number: the acquirer-side address lives in PaymentSplit.AccountRef.
type SplitPayee string

const (
	// SplitPayeeVenue is the restaurant. It receives the base amount — the
	// deposit, the pre-order or the ticket price.
	SplitPayeeVenue SplitPayee = "venue"
	// SplitPayeePlatform is BookEat. It receives the service fee and nothing
	// else — the same rule the ledger already follows (spec §9.2).
	SplitPayeePlatform SplitPayee = "platform"
)

// Valid reports whether p is a known payee.
func (p SplitPayee) Valid() bool {
	return p == SplitPayeeVenue || p == SplitPayeePlatform
}

// MaxPaymentSplits caps how many recipients one payment may be divided
// between.
//
// TipTop Pay's documentation advertises "неограниченное количество получателей"
// and states no limit, so this is OUR limit, not theirs: BookEat splits a
// payment two ways (venue + platform), and anything much larger than that is a
// bug in the caller — a loop that appended in the wrong place — not a business
// case. Catching it here costs one comparison; catching it at the acquirer
// costs a round trip and, if the acquirer happens to accept it, real money in
// the wrong accounts.
const MaxPaymentSplits = 10

// PaymentSplit is one recipient's share of a single payment.
type PaymentSplit struct {
	// Payee is the domain role of the recipient.
	Payee SplitPayee
	// AccountRef is the acquirer-side address of that recipient — for TipTop
	// Pay the sub-merchant's Public ID from the merchant cabinet, which the
	// adapter puts into Splits[].PublicId. It is an opaque handle, exactly like
	// PayoutDestination.Token: not a secret in the acquirer-key sense, but the
	// only address of somebody's money, so it is never logged in full.
	AccountRef string
	// Amount is that recipient's share, in minor units.
	Amount Money
}

// PaymentSplitPlan is the full division of one payment. Order is significant
// only for reproducibility (the same plan always serialises the same way).
type PaymentSplitPlan []PaymentSplit

// IsZero reports whether there is no split at all — an ordinary, single-payee
// payment. Every caller must treat this as "not a split payment", never as "a
// split of zero shares".
func (pl PaymentSplitPlan) IsZero() bool { return len(pl) == 0 }

// Total sums the shares. It returns ErrCurrencyMismatch rather than adding
// numbers in different currencies, and ErrMoneyOverflow instead of wrapping.
func (pl PaymentSplitPlan) Total() (Money, error) {
	if len(pl) == 0 {
		return Money{}, fmt.Errorf("empty split plan: %w", ErrValidation)
	}
	total := Money{Currency: pl[0].Amount.Currency}
	for _, s := range pl {
		if s.Amount.Currency != total.Currency {
			return Money{}, ErrCurrencyMismatch
		}
		if s.Amount.AmountMinor > 0 && total.AmountMinor > math.MaxInt64-s.Amount.AmountMinor {
			return Money{}, ErrMoneyOverflow
		}
		total.AmountMinor += s.Amount.AmountMinor
	}
	return total, nil
}

// Validate rejects, BEFORE any provider round trip, every split the acquirer
// would reject afterwards — plus the ones it might silently accept.
//
// Each failure carries its own ErrorCode so an operator reading a log (or the
// venue cabinet reading the API) can tell which of them fired without parsing a
// sentence. All of them wrap ErrValidation: nothing was charged, and no retry
// of the same request can succeed.
//
// The checks, in the order a caller most likely gets them wrong:
//
//   - the plan is empty (use IsZero to mean "no split", never an empty plan);
//   - more shares than MaxPaymentSplits;
//   - an unknown payee, or two shares for the same payee;
//   - a missing AccountRef — TipTop Pay answers "SubMerchant not found", but by
//     then we have already asked it to move money;
//   - a share that is zero or negative: a recipient that receives nothing must
//     be ABSENT from the plan, not present with a zero, because at
//     /payments/confirm an absent PublicId means "cancel that share" while a
//     zero-amount one is simply rejected;
//   - shares in a currency other than the payment's;
//   - a sum that is not exactly the total. This is the one that silently costs
//     money: TipTop Pay answers "Amount is not equal to request amount", and a
//     caller that rounded a percentage with a float is the classic way to get
//     there.
func (pl PaymentSplitPlan) Validate(total Money) error {
	if len(pl) == 0 {
		return WithCode(CodeSplitSumMismatch, fmt.Errorf("split plan is empty: %w", ErrValidation))
	}
	if len(pl) > MaxPaymentSplits {
		return WithCode(CodeSplitTooManyShares,
			fmt.Errorf("%d split shares, at most %d are allowed: %w", len(pl), MaxPaymentSplits, ErrValidation))
	}

	seenRef := make(map[string]struct{}, len(pl))
	seenPayee := make(map[SplitPayee]struct{}, len(pl))
	for i, s := range pl {
		if !s.Payee.Valid() {
			return WithCode(CodeSplitShareInvalid,
				fmt.Errorf("split share %d has unknown payee %q: %w", i, s.Payee, ErrValidation))
		}
		if _, dup := seenPayee[s.Payee]; dup {
			return WithCode(CodeSplitShareInvalid,
				fmt.Errorf("split share %d repeats payee %q: %w", i, s.Payee, ErrValidation))
		}
		seenPayee[s.Payee] = struct{}{}

		ref := strings.TrimSpace(s.AccountRef)
		if ref == "" {
			return WithCode(CodeSplitAccountMissing,
				fmt.Errorf("split share %d (%s) has no acquirer account: %w", i, s.Payee, ErrValidation))
		}
		if _, dup := seenRef[ref]; dup {
			// Never echo the ref itself: it addresses somebody's money.
			return WithCode(CodeSplitAccountDuplicate,
				fmt.Errorf("split share %d reuses the acquirer account of an earlier share: %w", i, ErrValidation))
		}
		seenRef[ref] = struct{}{}

		if s.Amount.AmountMinor <= 0 {
			return WithCode(CodeSplitShareInvalid,
				fmt.Errorf("split share %d (%s) is %s, it must be positive: %w", i, s.Payee, s.Amount, ErrValidation))
		}
		if s.Amount.Currency != total.Currency {
			return WithCode(CodeSplitShareInvalid,
				fmt.Errorf("split share %d (%s) is in %s, the payment is in %s: %w",
					i, s.Payee, s.Amount.Currency, total.Currency, ErrCurrencyMismatch))
		}
	}

	sum, err := pl.Total()
	if err != nil {
		return err
	}
	if sum.AmountMinor != total.AmountMinor {
		return WithCode(CodeSplitSumMismatch,
			fmt.Errorf("split shares add up to %s, the payment is %s: %w", sum, total, ErrValidation))
	}
	return nil
}

// BuildPaymentSplitPlan divides ONE payment between the venue and the platform
// using the division the rest of the payment layer already uses: the venue gets
// the base, the platform gets the service fee (see the file header).
//
// A zero fee produces a single-share plan rather than a share of zero — see
// Validate on why a zero share is never sent. A zero base cannot happen for a
// real payment (there is nothing to pay for) and is rejected.
//
// It does NOT call Validate: the caller does, against the payment's own total,
// so that the "shares must add up to the amount actually charged" check is
// always made against the number that will be on the wire.
func BuildPaymentSplitPlan(base, fee Money, venueRef, platformRef string) (PaymentSplitPlan, error) {
	if base.Currency != fee.Currency {
		return nil, ErrCurrencyMismatch
	}
	if base.AmountMinor <= 0 {
		return nil, WithCode(CodeSplitShareInvalid,
			fmt.Errorf("venue share %s must be positive: %w", base, ErrValidation))
	}
	if fee.AmountMinor < 0 {
		return nil, WithCode(CodeSplitShareInvalid,
			fmt.Errorf("platform share %s must not be negative: %w", fee, ErrValidation))
	}
	if strings.TrimSpace(venueRef) == "" {
		return nil, WithCode(CodeSplitAccountMissing,
			fmt.Errorf("venue has no acquirer sub-merchant account: %w", ErrValidation))
	}

	plan := PaymentSplitPlan{{Payee: SplitPayeeVenue, AccountRef: venueRef, Amount: base}}
	if fee.AmountMinor == 0 {
		return plan, nil
	}
	if strings.TrimSpace(platformRef) == "" {
		return nil, WithCode(CodeSplitAccountMissing,
			fmt.Errorf("platform has no acquirer sub-merchant account: %w", ErrValidation))
	}
	return append(plan, PaymentSplit{Payee: SplitPayeePlatform, AccountRef: platformRef, Amount: fee}), nil
}

// Scale re-divides the plan for a SMALLER total: a partial capture (the venue
// could not serve part of a pre-order) or a partial refund. Every recipient
// keeps its proportion of the new total, and the shares still add up EXACTLY —
// see DistributeProportionally for the rounding rule.
//
// Shares that scale down to zero are DROPPED, not sent as zero. For
// /payments/confirm that is precisely right and is the documented behaviour:
// "отсутствие сплит платежа в Splits ... приведет к отмене такого сплит платежа
// на всю его сумму" — a recipient whose share of the confirmed amount is
// nothing must indeed have its whole share cancelled. For /payments/refund an
// absent PublicId simply means "nothing goes back from that recipient".
//
// target must be positive and must not exceed the plan's own total: the
// acquirer answers "Amount is too big" for the latter, and there is no reason
// to pay for learning that.
func (pl PaymentSplitPlan) Scale(target Money) (PaymentSplitPlan, error) {
	total, err := pl.Total()
	if err != nil {
		return nil, err
	}
	if target.Currency != total.Currency {
		return nil, ErrCurrencyMismatch
	}
	if target.AmountMinor <= 0 {
		return nil, WithCode(CodeSplitShareInvalid,
			fmt.Errorf("split target %s must be positive: %w", target, ErrValidation))
	}
	if target.AmountMinor > total.AmountMinor {
		return nil, WithCode(CodeSplitAmountTooBig,
			fmt.Errorf("split target %s exceeds the split total %s: %w", target, total, ErrValidation))
	}
	if target.AmountMinor == total.AmountMinor {
		out := make(PaymentSplitPlan, len(pl))
		copy(out, pl)
		return out, nil
	}

	weights := make([]int64, len(pl))
	for i, s := range pl {
		weights[i] = s.Amount.AmountMinor
	}
	parts, err := DistributeProportionally(weights, target.AmountMinor)
	if err != nil {
		return nil, err
	}

	out := make(PaymentSplitPlan, 0, len(pl))
	for i, s := range pl {
		if parts[i] == 0 {
			continue
		}
		s.Amount = Money{AmountMinor: parts[i], Currency: target.Currency}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, WithCode(CodeSplitShareInvalid,
			fmt.Errorf("split target %s leaves every share at zero: %w", target, ErrValidation))
	}
	return out, nil
}

// DistributeProportionally splits target between len(weights) recipients in
// proportion to weights, in integer minor units, so that the parts sum to
// target EXACTLY.
//
// The method is largest-remainder (Hamilton): every recipient first gets the
// floor of its exact share, then the tiyn left over by all that flooring are
// handed out one each, to the recipients with the largest discarded fraction,
// ties broken by position so the result is deterministic.
//
// Why not "percentage of each, rounded up" (the rule ApplyBasisPoints uses):
// rounding each share independently does not preserve the sum. Three equal
// recipients of 100.00 ₸ rounded up get 33.34 each = 100.02 — two tiyn invented
// out of nothing, which the acquirer rejects as "Amount is not equal to request
// amount". Here the invariant that matters is not "who wins the fraction" but
// "the parts are the whole", because the whole is money that already exists.
//
// weights must all be non-negative and must not be all zero; target must be
// non-negative.
func DistributeProportionally(weights []int64, target int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("no weights to distribute over: %w", ErrValidation)
	}
	if target < 0 {
		return nil, ErrNegativeAmount
	}

	var totalWeight int64
	for _, w := range weights {
		if w < 0 {
			return nil, ErrNegativeAmount
		}
		if w > 0 && totalWeight > math.MaxInt64-w {
			return nil, ErrMoneyOverflow
		}
		totalWeight += w
	}
	if totalWeight == 0 {
		return nil, fmt.Errorf("cannot distribute over zero total weight: %w", ErrValidation)
	}

	out := make([]int64, len(weights))
	// Order the recipients by the fraction each one loses to flooring, largest
	// first; the remainder is handed out along this order.
	type leftover struct {
		idx       int
		remainder int64
	}
	rest := make([]leftover, 0, len(weights))

	var assigned int64
	for i, w := range weights {
		if w == 0 {
			continue
		}
		if target > 0 && w > math.MaxInt64/target {
			return nil, ErrMoneyOverflow
		}
		product := w * target
		out[i] = product / totalWeight
		assigned += out[i]
		rest = append(rest, leftover{idx: i, remainder: product % totalWeight})
	}

	sort.SliceStable(rest, func(a, b int) bool { return rest[a].remainder > rest[b].remainder })
	for i := 0; assigned < target; i++ {
		// len(rest) > 0 because totalWeight > 0, and the loop can need at most
		// len(rest)-1 extra units, so this never wraps around twice.
		out[rest[i%len(rest)].idx]++
		assigned++
	}
	return out, nil
}
