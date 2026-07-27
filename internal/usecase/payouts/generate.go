package payouts

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		// nil period: a manual generation settles "everything owed now" and
		// deliberately consumes no venue-local day, so it can never block the
		// scheduled end-of-day pass (see uq_payouts_venue_period, which ignores
		// NULL period_date rows).
		p, err := u.createOnePayout(ctx, restaurantID, dest, bal, nil)
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

// payoutPeriod is the venue-local day a scheduled payout settles. Date is a
// pure calendar label normalised to UTC midnight (so it never drifts with the
// server's zone); EndAt is the real instant that local day ended.
type payoutPeriod struct {
	Date  time.Time
	EndAt time.Time
	// ForcedByAge marks a period the daily pass settled under the max-hold rule
	// (the venue was below its threshold, but its oldest money had waited long
	// enough). It rides on the period rather than being a separate argument
	// because only a SCHEDULED payout can be forced — a manual generation
	// carries no period and therefore cannot claim it was age-driven.
	ForcedByAge bool
}

// createOnePayout writes one pending payout and its item claims in ONE
// transaction. If the item claim loses the race on any entry, the whole tx
// (payout row included) rolls back.
//
// period is nil for a manual generation and set for a scheduled end-of-day
// pass; when set, the row carries the venue's local day and the DB refuses a
// second live payout for it (uq_payouts_venue_period) — the once-per-day
// guarantee is the index, not this code.
//
// The acquirer's payout fee is computed HERE and frozen on the row: the amount
// actually dispatched depends on who bears it, and a payout that has already
// been generated must keep the cost and the policy it was generated under even
// if the tariff or the policy changes tomorrow.
func (u *UseCase) createOnePayout(ctx context.Context, restaurantID uuid.UUID, dest *domain.PayoutDestination, bal domain.OwedBalance, period *payoutPeriod) (*domain.Payout, error) {
	if bal.AmountMinor <= 0 || len(bal.Entries) == 0 {
		return nil, fmt.Errorf("%w: non-positive or empty owed balance", domain.ErrValidation)
	}
	// Ownership gate #1: the card handle about to be frozen on this payout must
	// be the card registered to THIS restaurant, and it must identify a card the
	// provider can address. Checked here rather than at each caller because
	// this is the ONLY place a destination becomes the address of real money —
	// the manual generation and the scheduled daily pass both come through it.
	if err := dest.VerifyOwnedBy(restaurantID); err != nil {
		return nil, err
	}
	gross := domain.Money{AmountMinor: bal.AmountMinor, Currency: bal.Currency}
	fee, err := domain.PayoutFee(gross, u.cfg.FeeBps, u.cfg.FeeMinimumMinor)
	if err != nil {
		return nil, fmt.Errorf("compute payout fee: %w", err)
	}
	net, err := domain.NetPayoutAmount(gross, fee, u.cfg.FeeBearer)
	if err != nil {
		// Only reachable under the venue-bears policy on a payout smaller than
		// the fee floor. The daily pass's minimum-payout guard normally keeps
		// such a balance rolling over instead; this is the last line of defence
		// so a nonsense transfer is never dispatched.
		return nil, fmt.Errorf("payout net amount for restaurant %s: %w", restaurantID, err)
	}

	now := u.now()
	id := uuid.New()
	p := &domain.Payout{
		ID:                     id,
		RestaurantID:           restaurantID,
		AmountMinor:            net.AmountMinor,
		GrossAmountMinor:       gross.AmountMinor,
		FeeMinor:               fee.AmountMinor,
		FeeBearer:              u.cfg.FeeBearer,
		Currency:               bal.Currency,
		Status:                 domain.PayoutPending,
		Method:                 dest.Method,
		DestinationToken:       dest.Token,
		DestinationCustomerRef: dest.ProviderCustomerRef,
		IdempotencyKey:         "payout:" + id.String(),
		StatusChangedAt:        now,
		CreatedAt:              now,
	}
	if period != nil {
		date, endAt := period.Date, period.EndAt
		p.PeriodDate, p.PeriodEndAt = &date, &endAt
		p.ForcedByAge = period.ForcedByAge
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
		// Create can now fail on TWO unique constraints, both mapped to
		// ErrAlreadyExists: the idempotency key, and — for a scheduled payout —
		// uq_payouts_venue_period, which is the losing tick in a two-worker
		// race for the same venue-day.
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
		"amount_minor", p.AmountMinor, "gross_minor", p.GrossAmountMinor,
		"fee_minor", p.FeeMinor, "fee_bearer", string(p.FeeBearer),
		"currency", string(p.Currency), "items", len(items), "period", periodLabel(period),
		"forced_by_age", p.ForcedByAge)
	return p, nil
}

// periodLabel renders a period for a log line; "manual" when there is none.
func periodLabel(period *payoutPeriod) string {
	if period == nil {
		return "manual"
	}
	return period.Date.Format(time.DateOnly)
}
