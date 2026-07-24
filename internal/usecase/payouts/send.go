package payouts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// SendPayout dispatches one pending payout to the acquirer. Superadmin only.
//
// Money-safety, identical in shape to CaptureOnSeating:
//
//  1. CAS pending→sent is claimed BEFORE the acquirer is ever called. A second
//     concurrent send finds the row no longer `pending`, gets ErrAlreadyExists,
//     and returns the current payout without calling the acquirer a second time
//     — the double-send guard.
//  2. The acquirer call carries pg_order_id = payout.ID and our idempotency
//     key, so even if step 1 somehow raced at two processes, the acquirer
//     itself resolves the repeated order id to ONE payout — a second guarantee.
//  3. A definite success → CAS sent→paid. A definite decline
//     (ErrProviderDeclined) → CAS sent→failed AND release the claimed ledger
//     entries in the same tx (so the money is owed again). A timeout/unknown
//     (ErrProviderOutcomeUnknown) leaves the payout `sent`, NEVER marked paid or
//     failed — the reconciler resolves it. Money is never guessed.
func (u *UseCase) SendPayout(ctx context.Context, actor Actor, payoutID uuid.UUID) (*domain.Payout, error) {
	if err := u.authorizeSuperadmin(actor); err != nil {
		return nil, err
	}
	p, err := u.payouts.GetByID(ctx, payoutID)
	if err != nil {
		return nil, err
	}
	// Idempotent replay: an already-sent/paid/failed payout is returned as-is,
	// no second dispatch.
	if p.Status != domain.PayoutPending {
		return p, nil
	}

	now := u.now()
	// Claim the send BEFORE calling the acquirer.
	if err := u.payouts.CompareAndSwapStatus(ctx, payoutID, domain.PayoutPending, domain.PayoutSent, domain.PayoutStatusPatch{}, now); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			// Someone else won the claim; return the current state.
			return u.payouts.GetByID(ctx, payoutID)
		}
		return nil, err
	}

	if u.gateway == nil {
		return nil, fmt.Errorf("%w: no payout gateway configured", domain.ErrProviderOutcomeUnknown)
	}
	if u.gateway.Name() != domain.ProviderFreedomPay {
		return nil, fmt.Errorf("%w: unsupported payout provider %q", domain.ErrValidation, u.gateway.Name())
	}

	resp, gwErr := u.gateway.Payout(ctx, domain.PayoutRequest{
		PayoutID:               payoutID,
		IdempotencyKey:         p.IdempotencyKey,
		Amount:                 p.Amount(),
		Method:                 p.Method,
		DestinationToken:       p.DestinationToken,
		DestinationCustomerRef: p.DestinationCustomerRef,
	})
	if gwErr != nil {
		return u.resolveSendError(ctx, payoutID, gwErr)
	}
	return u.resolveSendSuccess(ctx, payoutID, resp)
}

// resolveSendSuccess applies a definite acquirer answer.
func (u *UseCase) resolveSendSuccess(ctx context.Context, payoutID uuid.UUID, resp *domain.GatewayPayout) (*domain.Payout, error) {
	now := u.now()
	switch resp.Status {
	case domain.PayoutPaid:
		ref := resp.ProviderRef
		patch := domain.PayoutStatusPatch{}
		if ref != "" {
			patch.ProviderRef = &ref
		}
		if err := u.payouts.CompareAndSwapStatus(ctx, payoutID, domain.PayoutSent, domain.PayoutPaid, patch, now); err != nil {
			// A reconciler pass may have resolved it first — not a conflict.
			if errors.Is(err, domain.ErrAlreadyExists) {
				return u.payouts.GetByID(ctx, payoutID)
			}
			return nil, err
		}
	case domain.PayoutSent:
		// Accepted, still processing: persist the ref so the reconciler can
		// resolve it, leave the status `sent`.
		if resp.ProviderRef != "" {
			if err := u.payouts.SetProviderRef(ctx, payoutID, resp.ProviderRef); err != nil {
				return nil, err
			}
		}
	default:
		// The adapter should never return paid/sent-only here, but if it hands
		// back anything else we treat it as unknown and leave the payout `sent`.
		u.log.Warn("payout gateway returned an unexpected success status, leaving sent for reconciliation",
			"payout_id", payoutID, "status", string(resp.Status))
	}
	return u.payouts.GetByID(ctx, payoutID)
}

// resolveSendError applies an acquirer error. Only an explicit decline is
// recorded as failure; everything else is left `sent` for the reconciler.
func (u *UseCase) resolveSendError(ctx context.Context, payoutID uuid.UUID, gwErr error) (*domain.Payout, error) {
	if errors.Is(gwErr, domain.ErrProviderDeclined) {
		if err := u.markFailedAndRelease(ctx, payoutID, "declined", gwErr.Error()); err != nil {
			return nil, err
		}
		return u.payouts.GetByID(ctx, payoutID)
	}
	// Unknown outcome (timeout, malformed, transport): the money may already be
	// moving. Leave it `sent`; the reconciler will ask the acquirer.
	u.log.Warn("payout send outcome unknown, left sent for reconciliation",
		"payout_id", payoutID, "err", gwErr.Error())
	return nil, fmt.Errorf("payout %s dispatched, outcome unknown: %w", payoutID, gwErr)
}

// markFailedAndRelease moves a payout sent→failed and releases its claimed
// ledger entries in ONE transaction, so the money becomes owed again atomically
// with the failure. CAS-guarded: if a reconciler already resolved this payout,
// the CAS loses and the release is skipped.
func (u *UseCase) markFailedAndRelease(ctx context.Context, payoutID uuid.UUID, code, reason string) error {
	now := u.now()
	patch := domain.PayoutStatusPatch{FailureCode: &code, FailureReason: &reason}
	return u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payouts.CompareAndSwapStatus(ctx, payoutID, domain.PayoutSent, domain.PayoutFailed, patch, now); err != nil {
			return err
		}
		return u.items.DeleteByPayout(ctx, payoutID)
	})
}

// GenerateAndSendForRestaurant is the manual "generate + send for a period"
// trigger (increment 1). It generates the pending payouts, then sends each. An
// individual send that ends in an unknown outcome is logged and does not abort
// the others — that payout is safely left `sent` for the reconciler.
func (u *UseCase) GenerateAndSendForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.Payout, error) {
	generated, err := u.GenerateForRestaurant(ctx, actor, restaurantID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Payout, 0, len(generated))
	for i := range generated {
		sent, err := u.SendPayout(ctx, actor, generated[i].ID)
		if err != nil {
			// Unknown outcome: keep going, the payout is `sent` and reconcilable.
			u.log.Warn("payout send did not complete synchronously", "payout_id", generated[i].ID, "err", err.Error())
			cur, getErr := u.payouts.GetByID(ctx, generated[i].ID)
			if getErr == nil {
				out = append(out, *cur)
			}
			continue
		}
		out = append(out, *sent)
	}
	return out, nil
}

// ListForRestaurant returns a restaurant's payout history. RBAC:
// restaurant.manage (the venue may read its own statement) or superadmin.
func (u *UseCase) ListForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, limit int) ([]domain.Payout, error) {
	if err := u.authorizeRestaurant(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.payouts.List(ctx, restaurantID, limit)
}
