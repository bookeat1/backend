package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// CaptureUseCase converts a hold into a charge when the venue seats the
// guest (spec §6, scenario 3).
type CaptureUseCase interface {
	CaptureOnSeating(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.Payment, error)
}

// VoidUseCase releases a hold the venue never confirmed or explicitly
// rejected (spec §6, scenario 4).
type VoidUseCase interface {
	VoidOnRejection(ctx context.Context, actor Actor, bookingID uuid.UUID, reason string) (*domain.Payment, error)
}

type captureVoidUseCase struct {
	payments domain.PaymentRepository
	ledger   domain.PaymentLedgerRepository
	outbox   domain.PaymentOutboxRepository
	gateways gatewayResolver
	managers managerChecker
	tx       domain.TxManager
}

// NewCaptureUseCase and NewVoidUseCase share the same implementation and
// dependencies; they are exposed as two narrow interfaces so a caller only
// depends on the one action it performs (a manager-facing "seat the guest"
// handler has no business also being able to void a hold).

// NewCaptureUseCase constructs the seating-capture usecase.
func NewCaptureUseCase(
	payments domain.PaymentRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	managers managerChecker,
	tx domain.TxManager,
) CaptureUseCase {
	return &captureVoidUseCase{payments: payments, ledger: ledger, outbox: outbox, gateways: gateways, managers: managers, tx: tx}
}

// NewVoidUseCase constructs the hold-release usecase.
func NewVoidUseCase(
	payments domain.PaymentRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	managers managerChecker,
	tx domain.TxManager,
) VoidUseCase {
	return &captureVoidUseCase{payments: payments, outbox: outbox, gateways: gateways, managers: managers, tx: tx}
}

// CaptureOnSeating captures the booking's live hold in full. It is
// idempotent for a DEFINITE outcome: calling it again on an already-captured
// payment is a no-op, not an error — staff retrying a slow request must not
// risk a second capture attempt at the acquirer (the sandbox checklist's
// item #7 leaves "does a repeated clearing clear twice?" unconfirmed).
//
// It is emphatically NOT true, and the previous version of this comment was
// wrong to claim, that this usecase never calls Capture a second time for the
// same payment "no matter what" — see the second review's item #1 below. What
// it actually guarantees is: it never calls Capture a second time UNLESS the
// first call's outcome is definitely known to have failed. An UNKNOWN outcome
// (the acquirer's request timed out, or answered with a 5xx) leaves the
// payment parked in `capturing` on purpose, with no path back to `authorized`
// — a second CaptureOnSeating call on a `capturing` payment is refused by the
// status guard below, exactly like a second call on any other non-authorized
// status.
//
// Three race/ambiguity hazards, all fixed by a DB-level CAS claim BEFORE the
// acquirer call, never a check-then-write (report items #5, #6 and the second
// review's #1):
//
//  1. two concurrent CaptureOnSeating calls for the SAME booking (a double
//     click, a retried request) must not both call gw.Capture — only the
//     winner of the `authorized -> capturing` CAS may call the acquirer; the
//     loser's CAS fails with ErrAlreadyExists and it must not proceed;
//  2. a genuine race between this call and a webhook that already applied the
//     capture (webhook.go's applyCaptured) must NOT be reported to staff as
//     "capture failed, please retry manually" — that false alarm is exactly
//     what used to provoke a second, manual capture attempt. Losing the CAS
//     because the payment is ALREADY `captured` is success, not conflict;
//  3. an UNKNOWN acquirer outcome after the request actually went out (a
//     timeout, a 5xx, or an unparsable answer — domain.ErrProviderOutcomeUnknown)
//     must NOT be treated the same as an explicit decline
//     (domain.ErrProviderDeclined): releasing the `capturing` claim back to
//     `authorized` on an unknown outcome is exactly what let staff press
//     "seat the guest" a second time and clear the card twice, because the
//     first clearing may well have gone through at FreedomPay while its
//     response got lost on the way back. Only a DEFINITE decline releases the
//     claim; an unknown outcome leaves the payment in `capturing` until a
//     human or the (not-yet-built) reconciliation worker resolves it —
//     see domain.PaymentCapturing's doc comment. DO NOT run this usecase in
//     production before that worker exists.
func (u *captureVoidUseCase) CaptureOnSeating(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.Payment, error) {
	if !actor.staff() {
		return nil, fmt.Errorf("%w: only venue staff can capture a hold", domain.ErrForbidden)
	}
	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeStaffForRestaurant(ctx, u.managers, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	if p.Status == domain.PaymentCaptured {
		return p, nil
	}
	if p.Status != domain.PaymentAuthorized {
		return nil, fmt.Errorf("%w: payment is %s, cannot capture", domain.ErrInvalidStatus, p.Status)
	}
	if p.ProviderPaymentID == nil {
		return nil, fmt.Errorf("payment %s has no provider payment id: %w", p.ID, domain.ErrValidation)
	}

	claimedAt := time.Now()
	if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCapturing, claimedAt); err != nil {
		if !errors.Is(err, domain.ErrAlreadyExists) {
			return nil, err
		}
		// Lost the claim. Re-read to tell "a webhook already captured this
		// payment" (success, item #6) apart from "someone else is capturing
		// it right now" or "it moved to a different terminal state" (a real
		// conflict — do not retry blindly).
		current, rerr := u.payments.GetByID(ctx, p.ID)
		if rerr != nil {
			return nil, rerr
		}
		if current.Status == domain.PaymentCaptured {
			return current, nil
		}
		return nil, fmt.Errorf("%w: payment %s is %s, another capture attempt is already in flight",
			domain.ErrAlreadyExists, p.ID, current.Status)
	}

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		// The acquirer was never reached — nothing ambiguous happened, safe
		// to release the claim unconditionally.
		return nil, u.releaseCaptureClaim(ctx, p, err)
	}

	// External call, deliberately outside any DB transaction.
	if _, err := gw.Capture(ctx, *p.ProviderPaymentID, p.Total()); err != nil {
		wrapped := fmt.Errorf("capture with %s: %w", p.Provider, err)
		if errors.Is(err, domain.ErrProviderDeclined) {
			// A definite, explicit "no" from the acquirer (report item #1,
			// second review): the hold is unchanged, safe to release the
			// claim back to `authorized` so a retry or a Void can proceed.
			return nil, u.releaseCaptureClaim(ctx, p, wrapped)
		}
		// domain.ErrProviderOutcomeUnknown, or any error this usecase does
		// not specifically recognise: the acquirer MAY have already cleared
		// the card. Releasing the claim here is exactly the bug report item
		// #1 (second review) describes — it would let a retried seating
		// request call Capture a second time on money that may already be
		// taken. The payment is deliberately LEFT in `capturing`: the status
		// guard at the top of this method already refuses a second
		// CaptureOnSeating call on anything other than `authorized`, so no
		// further guard is needed here. Only the reconciliation worker
		// (KNOWN GAP, not built in this change) may resolve `capturing` by
		// reading gw.Get() — see domain.PaymentCapturing's doc comment. DO
		// NOT run this usecase in production before that worker exists.
		logging.FromContext(ctx).Error("payment.capture_outcome_unknown",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return nil, wrapped
	}

	now := time.Now()
	entries := captureLedgerEntries(*p, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return nil, err
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCapturing, domain.PaymentCaptured, now); err != nil {
			return err
		}
		if err := u.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		p.Status = domain.PaymentCaptured
		p.CapturedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentCaptured, now)
	})
	if err != nil {
		// Non-blocking item #2 (second review): the final CAS can lose for a
		// reason that is NOT "our own commit failed" — a webhook may have
		// applied this exact capture (webhook.go's applyCaptured) between our
		// successful gw.Capture call above and this commit. That is success,
		// not a false alarm; the start-of-method claim already tells the two
		// apart (report item #6) and this final claim must do the same,
		// otherwise staff sees a spurious "needs reconciliation" error and is
		// tempted to retry a capture that already completed.
		if errors.Is(err, domain.ErrAlreadyExists) {
			current, rerr := u.payments.GetByID(ctx, p.ID)
			if rerr != nil {
				return nil, rerr
			}
			if current.Status == domain.PaymentCaptured {
				return current, nil
			}
		}
		// Money WAS taken at the acquirer — the call above succeeded — but
		// our own commit failed afterwards. This must NOT be papered over by
		// retrying Capture (unconfirmed idempotency on the acquirer side);
		// it needs the reconciliation worker (KNOWN GAP, see the final
		// report) reading gw.Get() to find the truth and fix the local row.
		logging.FromContext(ctx).Error("payment.capture_commit_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return nil, fmt.Errorf("capture succeeded at %s but local commit failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, err)
	}

	logging.FromContext(ctx).Info(logging.EventPaymentCaptured,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
	)
	return p, nil
}

// releaseCaptureClaim reverts the `capturing` claim back to `authorized` when
// the acquirer call itself never happened or failed — the hold is unchanged,
// so a later retry (or a Void) must find the payment back in `authorized`,
// not stuck in a transient state forever. Returns origErr (wrapped, never
// swallowed) regardless of whether the release itself succeeds.
func (u *captureVoidUseCase) releaseCaptureClaim(ctx context.Context, p *domain.Payment, origErr error) error {
	releasedAt := time.Now()
	if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCapturing, domain.PaymentAuthorized, releasedAt); err != nil {
		logging.FromContext(ctx).Error("payment.capture_claim_release_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
	}
	return origErr
}

// VoidOnRejection releases the booking's live hold. Idempotent for the same
// reason as CaptureOnSeating: calling it twice on an already-voided payment
// is a no-op.
//
// Symmetric to CaptureOnSeating (second review, non-blocking item #1): before
// this fix, VoidOnRejection called gw.Void with no local claim at all, unlike
// CaptureOnSeating's `authorized -> capturing` CAS — two concurrent "release
// the hold" requests for the SAME booking would both reach the acquirer. It
// now claims `authorized -> voiding` first, exactly like capture, and
// resolves the claim the same way: a definite decline
// (domain.ErrProviderDeclined) releases it back to `authorized`; an unknown
// outcome (domain.ErrProviderOutcomeUnknown) leaves it in `voiding` for the
// same reconciliation-worker reason as `capturing` (see
// domain.PaymentVoiding's doc comment — DO NOT run in production before that
// worker exists). Losing the claim because a webhook already voided this
// payment (report item #6's reasoning, applied here too, non-blocking item
// #2) is reported as success, not as a false-alarm conflict.
func (u *captureVoidUseCase) VoidOnRejection(ctx context.Context, actor Actor, bookingID uuid.UUID, reason string) (*domain.Payment, error) {
	if !actor.staff() {
		return nil, fmt.Errorf("%w: only venue staff can release a hold", domain.ErrForbidden)
	}
	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeStaffForRestaurant(ctx, u.managers, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	if p.Status == domain.PaymentVoided {
		return p, nil
	}
	if p.Status != domain.PaymentAuthorized {
		return nil, fmt.Errorf("%w: payment is %s, cannot void", domain.ErrInvalidStatus, p.Status)
	}
	if p.ProviderPaymentID == nil {
		return nil, fmt.Errorf("payment %s has no provider payment id: %w", p.ID, domain.ErrValidation)
	}

	claimedAt := time.Now()
	if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentVoiding, claimedAt); err != nil {
		if !errors.Is(err, domain.ErrAlreadyExists) {
			return nil, err
		}
		// Lost the claim. Re-read to tell "a webhook already voided this
		// payment" (success) apart from "someone else is voiding it right
		// now" or "it moved to a different terminal state" (a real conflict).
		current, rerr := u.payments.GetByID(ctx, p.ID)
		if rerr != nil {
			return nil, rerr
		}
		if current.Status == domain.PaymentVoided {
			return current, nil
		}
		return nil, fmt.Errorf("%w: payment %s is %s, another void attempt is already in flight",
			domain.ErrAlreadyExists, p.ID, current.Status)
	}

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		// The acquirer was never reached — nothing ambiguous, safe to
		// release the claim unconditionally.
		return nil, u.releaseVoidClaim(ctx, p, err)
	}

	// External call, deliberately outside any DB transaction.
	if err := gw.Void(ctx, *p.ProviderPaymentID); err != nil {
		wrapped := fmt.Errorf("void with %s: %w", p.Provider, err)
		if errors.Is(err, domain.ErrProviderDeclined) {
			return nil, u.releaseVoidClaim(ctx, p, wrapped)
		}
		// Unknown outcome: leave the payment in `voiding`, same reasoning as
		// CaptureOnSeating's unknown-outcome branch above.
		logging.FromContext(ctx).Error("payment.void_outcome_unknown",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return nil, wrapped
	}

	now := time.Now()
	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentVoiding, domain.PaymentVoided, now); err != nil {
			return err
		}
		p.Status = domain.PaymentVoided
		p.VoidedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentVoided, now)
	})
	if err != nil {
		// Same reasoning as CaptureOnSeating's final CAS (non-blocking item
		// #2, applied here for consistency): losing this CAS can mean a
		// webhook already applied this exact void, not that our own commit
		// failed after the acquirer call succeeded.
		if errors.Is(err, domain.ErrAlreadyExists) {
			current, rerr := u.payments.GetByID(ctx, p.ID)
			if rerr != nil {
				return nil, rerr
			}
			if current.Status == domain.PaymentVoided {
				return current, nil
			}
		}
		logging.FromContext(ctx).Error("payment.void_commit_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return nil, fmt.Errorf("void succeeded at %s but local commit failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, err)
	}

	logging.FromContext(ctx).Info(logging.EventPaymentVoided,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("reason", reason),
	)
	return p, nil
}

// releaseVoidClaim reverts the `voiding` claim back to `authorized` when the
// acquirer call itself never happened or was definitively declined — the
// hold is unchanged, so a later retry must find the payment back in
// `authorized`, not stuck in a transient state forever. Symmetric to
// releaseCaptureClaim. Returns origErr (wrapped, never swallowed) regardless
// of whether the release itself succeeds.
func (u *captureVoidUseCase) releaseVoidClaim(ctx context.Context, p *domain.Payment, origErr error) error {
	releasedAt := time.Now()
	if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentVoiding, domain.PaymentAuthorized, releasedAt); err != nil {
		logging.FromContext(ctx).Error("payment.void_claim_release_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
	}
	return origErr
}
