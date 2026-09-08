package domain

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RestaurantSplitAccount is one venue's identity at ONE acquirer for split
// payments: the sub-merchant account the acquirer credits with that venue's
// share of a guest's payment (TipTop Pay calls it the sub-merchant Public ID,
// issued in the merchant cabinet after the venue is onboarded under our
// marketplace terminal).
//
// It is deliberately NOT a column on `restaurants`, and not part of
// PayoutDestination either:
//
//   - it is per ACQUIRER, so a venue may have one at TipTop Pay and none at
//     FreedomPay; a column could hold only one of them;
//   - a payout destination is where we send money we already hold. A split
//     account is where the acquirer sends money we never hold. Merging the two
//     would make "which of these two addresses did this tenge take" a guess;
//   - `restaurants` is the hottest read in the product and a money knob read
//     once per checkout does not belong in it (same reasoning as
//     restaurant_payout_settings, migration 0053).
//
// AccountRef is an opaque handle, like PayoutDestination.Token: not a secret in
// the acquirer-key sense, but the only address of somebody's money — never
// logged in full.
type RestaurantSplitAccount struct {
	RestaurantID uuid.UUID
	Provider     PaymentProvider
	AccountRef   string
	// IsActive lets a venue be suspended from split payments without deleting
	// the row and losing which account historic payments were split to. An
	// inactive account is treated exactly like a missing one by the checkout.
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate enforces the invariants that make a split account usable at all.
func (a RestaurantSplitAccount) Validate() error {
	if a.RestaurantID == uuid.Nil {
		return WithCode(CodeSplitAccountMissing, ErrValidation)
	}
	if !a.Provider.Valid() {
		return WithCode(CodeSplitAccountMissing, ErrValidation)
	}
	if strings.TrimSpace(a.AccountRef) == "" {
		return WithCode(CodeSplitAccountMissing, ErrValidation)
	}
	return nil
}

// RestaurantSplitAccountRepository stores the venue↔sub-merchant mapping.
//
// GetActive returns ErrNotFound both when the venue was never onboarded at that
// acquirer and when its account is deactivated. The checkout must treat that as
// a hard stop for a split payment, never as "then send the whole amount to the
// marketplace terminal" — that would pay the platform the venue's money.
type RestaurantSplitAccountRepository interface {
	GetActive(ctx context.Context, provider PaymentProvider, restaurantID uuid.UUID) (*RestaurantSplitAccount, error)
	// Upsert writes the mapping for (restaurant, provider).
	Upsert(ctx context.Context, a *RestaurantSplitAccount) error
}
