package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// Split payments — deciding the division, before the acquirer is called
//
// The division itself is not new and is not invented here: a payment is already
// BaseAmountMinor (the venue's) + FeeMinor (the platform's commission, grossed
// up in create.go via domain.GrossUpForAcquirerWithMinimum), and the ledger
// already books exactly those two numbers to AccountRestaurant and
// AccountPlatform. A split payment only asks the acquirer to make that same
// division at the moment of the charge instead of afterwards.
//
// What IS new is the address: each recipient needs an acquirer-side
// sub-merchant account. As of 2026-08-20 no venue has one — they are issued as
// venues are onboarded to acquiring — so the whole feature is off by default
// and, when it is on, a venue without an account cannot be charged at all.
// ---------------------------------------------------------------------------

// splitAccountReader resolves a venue's acquirer-side sub-merchant account
// (restaurant_split_accounts, migration 0077). It returns domain.ErrNotFound
// both for a venue that was never onboarded and for one whose account is
// deactivated — see domain.RestaurantSplitAccountRepository.
type splitAccountReader interface {
	GetActive(ctx context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error)
}

// resolveSplitPlan decides how ONE payment is divided at the acquirer.
//
// It returns an empty plan (and no error) when split payments are not in play
// for this deployment — that is the ordinary, single-recipient payment this
// service has always made, and it stays the default.
//
// When splits ARE enabled, a venue without an active sub-merchant account is a
// HARD STOP: the payment is refused before the acquirer is called, with
// CodeSplitAccountMissing on top of domain.ErrUnavailable ("this venue is not
// set up to take payments yet"). The alternative — sending the charge without
// splits — would put the venue's money on the platform's account and leave
// somebody to find it in a statement weeks later. Owner's decision, 2026-08-20,
// and it is the same rule the adapter enforces again on its own side.
func (u *createUseCase) resolveSplitPlan(
	ctx context.Context,
	provider domain.PaymentProvider,
	restaurantID uuid.UUID,
	base, fee, total domain.Money,
) (domain.PaymentSplitPlan, error) {
	if !u.cfg.SplitEnabled || u.splitAccounts == nil {
		return nil, nil
	}

	account, err := u.splitAccounts.GetActive(ctx, provider, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.WithCode(domain.CodeSplitAccountMissing, fmt.Errorf(
				"restaurant %s has no active %s sub-merchant account, split payments cannot be taken for it: %w",
				restaurantID, provider, domain.ErrUnavailable))
		}
		return nil, err
	}

	plan, err := domain.BuildPaymentSplitPlan(base, fee, account.AccountRef, u.cfg.PlatformSplitAccountRef)
	if err != nil {
		return nil, err
	}
	// Validated against the amount that will actually be charged, not against
	// base+fee computed a second time: this is the check that catches a future
	// change to the gross-up before it becomes "Amount is not equal to request
	// amount" from the acquirer.
	if err := plan.Validate(total); err != nil {
		return nil, err
	}
	return plan, nil
}
