package domain

import (
	"errors"
	"testing"
)

const (
	venueAccount    = "sub_merchant_venue"
	platformAccount = "sub_merchant_platform"
)

func twoShares(baseMinor, feeMinor int64) PaymentSplitPlan {
	return PaymentSplitPlan{
		{Payee: SplitPayeeVenue, AccountRef: venueAccount, Amount: KZT(baseMinor)},
		{Payee: SplitPayeePlatform, AccountRef: platformAccount, Amount: KZT(feeMinor)},
	}
}

func TestPaymentSplitPlanValidateHappyPath(t *testing.T) {
	// The real shape of a BookEat payment: a 10 000 ₸ deposit grossed up for a
	// 3,5 % acquirer — total 10 362,70 ₸, of which 362,70 ₸ is the fee.
	plan := twoShares(1_000_000, 36_270)
	if err := plan.Validate(KZT(1_036_270)); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	total, err := plan.Total()
	if err != nil {
		t.Fatalf("Total() = %v", err)
	}
	if total.AmountMinor != 1_036_270 {
		t.Fatalf("total = %d, want 1036270", total.AmountMinor)
	}
}

func TestPaymentSplitPlanValidateRejections(t *testing.T) {
	tooMany := make(PaymentSplitPlan, 0, MaxPaymentSplits+1)
	for i := 0; i <= MaxPaymentSplits; i++ {
		tooMany = append(tooMany, PaymentSplit{
			Payee:      SplitPayeeVenue,
			AccountRef: string(rune('a'+i)) + "_account",
			Amount:     KZT(100),
		})
	}

	tests := []struct {
		name     string
		plan     PaymentSplitPlan
		total    Money
		wantCode ErrorCode
	}{
		{
			// The expensive one: TipTopPay answers "Amount is not equal to
			// request amount" and nothing is charged — but only after a round
			// trip that this check makes unnecessary.
			name:     "shares do not add up to the total",
			plan:     twoShares(1_000_000, 36_269),
			total:    KZT(1_036_270),
			wantCode: CodeSplitSumMismatch,
		},
		{
			name:     "shares add up to more than the total",
			plan:     twoShares(1_000_000, 36_271),
			total:    KZT(1_036_270),
			wantCode: CodeSplitSumMismatch,
		},
		{
			name: "a share has no acquirer account",
			plan: PaymentSplitPlan{
				{Payee: SplitPayeeVenue, AccountRef: "  ", Amount: KZT(1_000_000)},
				{Payee: SplitPayeePlatform, AccountRef: platformAccount, Amount: KZT(36_270)},
			},
			total:    KZT(1_036_270),
			wantCode: CodeSplitAccountMissing,
		},
		{
			name: "two shares address the same account",
			plan: PaymentSplitPlan{
				{Payee: SplitPayeeVenue, AccountRef: venueAccount, Amount: KZT(1_000_000)},
				{Payee: SplitPayeePlatform, AccountRef: venueAccount, Amount: KZT(36_270)},
			},
			total:    KZT(1_036_270),
			wantCode: CodeSplitAccountDuplicate,
		},
		{
			// A recipient that gets nothing must be ABSENT, not zero: at
			// /payments/confirm an absent PublicId cancels that share, which is
			// the intended meaning; a zero one is simply refused.
			name:     "a share is zero",
			plan:     twoShares(1_036_270, 0),
			total:    KZT(1_036_270),
			wantCode: CodeSplitShareInvalid,
		},
		{
			name:     "a share is negative",
			plan:     twoShares(1_072_540, -36_270),
			total:    KZT(1_036_270),
			wantCode: CodeSplitShareInvalid,
		},
		{
			name: "a share is in another currency",
			plan: PaymentSplitPlan{
				{Payee: SplitPayeeVenue, AccountRef: venueAccount, Amount: KZT(1_000_000)},
				{Payee: SplitPayeePlatform, AccountRef: platformAccount, Amount: Money{AmountMinor: 36_270, Currency: Currency("USD")}},
			},
			total:    KZT(1_036_270),
			wantCode: CodeSplitShareInvalid,
		},
		{
			name: "the same payee twice",
			plan: PaymentSplitPlan{
				{Payee: SplitPayeeVenue, AccountRef: venueAccount, Amount: KZT(500_000)},
				{Payee: SplitPayeeVenue, AccountRef: platformAccount, Amount: KZT(536_270)},
			},
			total:    KZT(1_036_270),
			wantCode: CodeSplitShareInvalid,
		},
		{
			name:     "more shares than we allow",
			plan:     tooMany,
			total:    KZT(int64(len(tooMany)) * 100),
			wantCode: CodeSplitTooManyShares,
		},
		{
			name:     "an empty plan is not a split of nothing",
			plan:     PaymentSplitPlan{},
			total:    KZT(1_036_270),
			wantCode: CodeSplitSumMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate(tc.total)
			if err == nil {
				t.Fatalf("Validate() = nil, want a rejection")
			}
			code, ok := CodeOf(err)
			if !ok || code != tc.wantCode {
				t.Fatalf("code = %q (present=%v), want %q — err: %v", code, ok, tc.wantCode, err)
			}
			// Every one of these is a caller mistake, not a provider outcome:
			// nothing was sent anywhere and no retry of the same request helps.
			if !errors.Is(err, ErrValidation) && !errors.Is(err, ErrCurrencyMismatch) {
				t.Fatalf("err = %v, want it to wrap ErrValidation or ErrCurrencyMismatch", err)
			}
		})
	}
}

func TestBuildPaymentSplitPlanUsesBaseAndFee(t *testing.T) {
	plan, err := BuildPaymentSplitPlan(KZT(1_000_000), KZT(36_270), venueAccount, platformAccount)
	if err != nil {
		t.Fatalf("BuildPaymentSplitPlan() = %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("len(plan) = %d, want 2", len(plan))
	}
	if plan[0].Payee != SplitPayeeVenue || plan[0].Amount.AmountMinor != 1_000_000 {
		t.Fatalf("venue share = %+v, want the base", plan[0])
	}
	if plan[1].Payee != SplitPayeePlatform || plan[1].Amount.AmountMinor != 36_270 {
		t.Fatalf("platform share = %+v, want the fee", plan[1])
	}
	if err := plan.Validate(KZT(1_036_270)); err != nil {
		t.Fatalf("built plan does not validate: %v", err)
	}
}

func TestBuildPaymentSplitPlanZeroFeeIsOneShare(t *testing.T) {
	// A venue on a 0 bps rate: the platform's share is nothing, so it must not
	// appear at all rather than appear as a zero the acquirer rejects. The
	// platform account is not even needed in that case.
	plan, err := BuildPaymentSplitPlan(KZT(500_000), KZT(0), venueAccount, "")
	if err != nil {
		t.Fatalf("BuildPaymentSplitPlan() = %v", err)
	}
	if len(plan) != 1 || plan[0].Payee != SplitPayeeVenue {
		t.Fatalf("plan = %+v, want a single venue share", plan)
	}
	if err := plan.Validate(KZT(500_000)); err != nil {
		t.Fatalf("single-share plan does not validate: %v", err)
	}
}

func TestBuildPaymentSplitPlanMissingAccounts(t *testing.T) {
	if _, err := BuildPaymentSplitPlan(KZT(1_000_000), KZT(36_270), "", platformAccount); err == nil {
		t.Fatalf("a venue with no sub-merchant account must not produce a plan")
	} else if code, _ := CodeOf(err); code != CodeSplitAccountMissing {
		t.Fatalf("code = %q, want %q", code, CodeSplitAccountMissing)
	}
	if _, err := BuildPaymentSplitPlan(KZT(1_000_000), KZT(36_270), venueAccount, " "); err == nil {
		t.Fatalf("a non-zero fee with no platform account must not produce a plan")
	} else if code, _ := CodeOf(err); code != CodeSplitAccountMissing {
		t.Fatalf("code = %q, want %q", code, CodeSplitAccountMissing)
	}
}

// TestDistributeProportionallyNeverLosesOrInventsATiyn is the rounding
// property: whatever the weights and whatever the target, the parts are exactly
// the whole. A commission that does not divide evenly is the normal case, not
// the exotic one.
func TestDistributeProportionallyNeverLosesOrInventsATiyn(t *testing.T) {
	tests := []struct {
		name    string
		weights []int64
		target  int64
		want    []int64
	}{
		{
			// The classic float trap: three equal shares of 100,00 ₸. Rounding
			// each one up gives 33,34 × 3 = 100,02 — two tiyn out of nowhere.
			name:    "three equal shares of an indivisible total",
			weights: []int64{1, 1, 1},
			target:  10_000,
			want:    []int64{3_334, 3_333, 3_333},
		},
		{
			// A real capture: the venue's 10 000 ₸ and our 362,70 ₸ fee, of
			// which only half is actually confirmed.
			name:    "half a grossed-up deposit",
			weights: []int64{1_000_000, 36_270},
			target:  518_135,
			want:    []int64{500_000, 18_135},
		},
		{
			name:    "a target of one tiyn goes to the largest remainder",
			weights: []int64{1_000_000, 36_270},
			target:  1,
			want:    []int64{1, 0},
		},
		{
			name:    "a zero-weight recipient gets nothing",
			weights: []int64{100, 0, 100},
			target:  101,
			want:    []int64{51, 0, 50},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DistributeProportionally(tc.weights, tc.target)
			if err != nil {
				t.Fatalf("DistributeProportionally() = %v", err)
			}
			var sum int64
			for i, part := range got {
				if part < 0 {
					t.Fatalf("part %d is negative: %d", i, part)
				}
				sum += part
			}
			if sum != tc.target {
				t.Fatalf("parts sum to %d, target is %d — money was %s", sum, tc.target,
					map[bool]string{true: "invented", false: "lost"}[sum > tc.target])
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestDistributeProportionallySumsExactlyOverManyTargets is the same property
// stated as an exhaustive sweep rather than as examples: for a commission of
// 3,5 % grossed up, every possible partial amount must still add up.
func TestDistributeProportionallySumsExactlyOverManyTargets(t *testing.T) {
	weights := []int64{1_000_000, 36_270}
	for target := int64(1); target <= 1_036_270; target += 997 {
		parts, err := DistributeProportionally(weights, target)
		if err != nil {
			t.Fatalf("target %d: %v", target, err)
		}
		if sum := parts[0] + parts[1]; sum != target {
			t.Fatalf("target %d: parts %v sum to %d", target, parts, sum)
		}
	}
}

func TestPaymentSplitPlanScale(t *testing.T) {
	plan := twoShares(1_000_000, 36_270)

	t.Run("the full amount keeps every share", func(t *testing.T) {
		scaled, err := plan.Scale(KZT(1_036_270))
		if err != nil {
			t.Fatalf("Scale() = %v", err)
		}
		if err := scaled.Validate(KZT(1_036_270)); err != nil {
			t.Fatalf("scaled plan does not validate: %v", err)
		}
	})

	t.Run("a partial amount still adds up", func(t *testing.T) {
		scaled, err := plan.Scale(KZT(518_135))
		if err != nil {
			t.Fatalf("Scale() = %v", err)
		}
		if err := scaled.Validate(KZT(518_135)); err != nil {
			t.Fatalf("scaled plan does not validate: %v", err)
		}
	})

	t.Run("a share that scales to zero is dropped, not sent as zero", func(t *testing.T) {
		scaled, err := plan.Scale(KZT(1))
		if err != nil {
			t.Fatalf("Scale() = %v", err)
		}
		if len(scaled) != 1 || scaled[0].Payee != SplitPayeeVenue {
			t.Fatalf("scaled = %+v, want only the venue share", scaled)
		}
		if err := scaled.Validate(KZT(1)); err != nil {
			t.Fatalf("scaled plan does not validate: %v", err)
		}
	})

	t.Run("more than the split is worth is refused here, not by the acquirer", func(t *testing.T) {
		_, err := plan.Scale(KZT(1_036_271))
		if err == nil {
			t.Fatalf("Scale() = nil, want a rejection")
		}
		if code, _ := CodeOf(err); code != CodeSplitAmountTooBig {
			t.Fatalf("code = %q, want %q", code, CodeSplitAmountTooBig)
		}
	})

	t.Run("zero is not a partial amount", func(t *testing.T) {
		if _, err := plan.Scale(KZT(0)); err == nil {
			t.Fatalf("Scale(0) = nil, want a rejection")
		}
	})
}
