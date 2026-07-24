package payouts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// GenerateForRestaurant computes what BookEat owes a restaurant from the ledger
// and creates one PENDING payout per currency with a positive balance, claiming
// exactly the unpaid ledger entries into it. It does NOT send — SendPayout is a
// separate, individually idempotent step.
//
// Superadmin only: this is a money-OUT platform operation. Increment 1 settles
// the whole unpaid balance; a date-bounded "period" is a documented extension
// (filter OwedForRestaurant by created_at) and an automatic schedule would call
// this exact method from a worker tick — see the package/PR notes.
//
// Idempotency: two concurrent generations for the same restaurant both try to
// claim the same ledger entries; the loser's CreateBatch hits
// uq_payout_items_ledger_entry and the WHOLE tx rolls back (payout row +
// items), so a ledger entry is never in two payouts and no orphan payout is
// left behind.
func (u *UseCase) GenerateForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.Payout, error) {
	if err := u.authorizeSuperadmin(actor); err != nil {
		return nil, err
	}
	dest, err := u.destinations.Get(ctx, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: restaurant has no payout destination", domain.ErrNotFound)
		}
		return nil, err
	}
	balances, err := u.owed.OwedForRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, err
	}

	var created []domain.Payout
	for _, bal := range balances {
		p, err := u.createOnePayout(ctx, restaurantID, dest, bal)
		if err != nil {
			// A concurrent generation claimed these entries first: skip this
			// currency, it is now owed by that other payout. Not an error.
			if errors.Is(err, domain.ErrAlreadyExists) {
				u.log.Info("payout generation lost claim race, skipping",
					"restaurant_id", restaurantID, "currency", string(bal.Currency))
				continue
			}
			return nil, err
		}
		created = append(created, *p)
	}
	return created, nil
}

// GenerateAll runs GenerateForRestaurant for every restaurant that currently
// has a positive unpaid balance. A restaurant without a payout destination is
// logged and skipped rather than failing the whole run.
func (u *UseCase) GenerateAll(ctx context.Context, actor Actor) ([]domain.Payout, error) {
	if err := u.authorizeSuperadmin(actor); err != nil {
		return nil, err
	}
	ids, err := u.owed.OwedRestaurantIDs(ctx)
	if err != nil {
		return nil, err
	}
	var created []domain.Payout
	for _, id := range ids {
		payouts, err := u.GenerateForRestaurant(ctx, actor, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				u.log.Warn("skipping restaurant with owed balance but no payout destination", "restaurant_id", id)
				continue
			}
			return created, err
		}
		created = append(created, payouts...)
	}
	return created, nil
}

// createOnePayout writes one pending payout and its item claims in ONE
// transaction. If the item claim loses the race on any entry, the whole tx
// (payout row included) rolls back.
func (u *UseCase) createOnePayout(ctx context.Context, restaurantID uuid.UUID, dest *domain.PayoutDestination, bal domain.OwedBalance) (*domain.Payout, error) {
	if bal.AmountMinor <= 0 || len(bal.Entries) == 0 {
		return nil, fmt.Errorf("%w: non-positive or empty owed balance", domain.ErrValidation)
	}
	now := u.now()
	id := uuid.New()
	p := &domain.Payout{
		ID:                     id,
		RestaurantID:           restaurantID,
		AmountMinor:            bal.AmountMinor,
		Currency:               bal.Currency,
		Status:                 domain.PayoutPending,
		Method:                 dest.Method,
		DestinationToken:       dest.Token,
		DestinationCustomerRef: dest.ProviderCustomerRef,
		IdempotencyKey:         "payout:" + id.String(),
		StatusChangedAt:        now,
		CreatedAt:              now,
	}
	items := make([]domain.PayoutItem, 0, len(bal.Entries))
	for _, e := range bal.Entries {
		items = append(items, domain.PayoutItem{
			PayoutID:          id,
			LedgerEntryID:     e.LedgerEntryID,
			RestaurantID:      restaurantID,
			AmountSignedMinor: e.AmountSignedMinor,
			Currency:          e.Currency,
		})
	}

	if err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payouts.Create(ctx, p); err != nil {
			return err
		}
		// The claim is the money-safety arbiter: a unique_violation here
		// (ErrAlreadyExists) means another payout already owns one of these
		// ledger entries, and rolling back removes the payout row we just wrote.
		return u.items.CreateBatch(ctx, items)
	}); err != nil {
		return nil, err
	}
	u.log.Info("payout generated",
		"payout_id", id, "restaurant_id", restaurantID,
		"amount_minor", p.AmountMinor, "currency", string(p.Currency), "items", len(items))
	return p, nil
}
