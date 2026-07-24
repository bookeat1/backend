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

// DepositCancellationUseCase settles a booking's HELD deposit when the booking
// is cancelled or marked no-show. A deposit (domain.PurposeDeposit) is only
// ever a hold (Authorize) up to this point — it is deliberately NOT captured on
// creation, unlike a pre-order — so the money decision is "release the hold" vs
// "capture the hold", never a refund of already-taken money. The owner-confirmed
// policy (this is a money/booking-path change — route through code review):
//
//	trigger / timing                                  held deposit outcome
//	guest cancels EARLIER than free_cancel_window      VOID    (guest released)
//	guest cancels LATER than free_cancel_window        CAPTURE (venue keeps it)
//	no-show                                            CAPTURE (same as a late cancel)
//	venue cancels (any time)                           VOID    (guest released)
//
// The free-cancellation window is per-restaurant
// (restaurants.free_cancel_window_minutes, migration 0034/0035, owner-confirmed
// default 120 minutes) and is resolved through the SAME cancelDeadlineResolver
// RefundUseCase.Settle uses, so every money decision reads one window.
//
// It reuses the exact CAS-guarded capture/void mechanics of CaptureOnSeating /
// VoidOnRejection (captureHold / voidHold in capture.go) rather than writing an
// ad-hoc status change, so the idempotency and race-safety proven there hold
// here too: a double cancel, or a manual cancel racing the no-show worker, can
// only ever win one authorized→(capturing|voiding) CAS — the loser finds the
// deposit already settled and moves no money a second time.
type DepositCancellationUseCase interface {
	SettleDepositOnCancel(ctx context.Context, actor Actor, bookingID uuid.UUID, in DepositCancelInput) (*domain.Payment, error)
}

// DepositCancelInput carries what only the caller (the cancel / no-show
// transition) knows: which outcome fired and, for a guest cancellation, when it
// happened. CancelledAt is server-supplied by the booking transition (its own
// CancelledAt) — a guest never gets to assert their own "in time" moment here.
type DepositCancelInput struct {
	Trigger domain.RefundTrigger
	// CancelledAt is when the booking was cancelled, for a guest cancellation.
	// nil means "now" (the natural value for a no-show, which is decided at the
	// moment the visit window lapses). It is only consulted for
	// RefundTriggerGuestCancel; the other triggers do not depend on timing.
	CancelledAt *time.Time
	Reason      *string
}

type depositCancellationUseCase struct {
	payments domain.PaymentRepository
	capvoid  *captureVoidUseCase
	refunds  RefundUseCase
	bookings bookingReader
	managers managerChecker
	deadline cancelDeadlineResolver
}

// systemActor performs the money moves that are a CONSEQUENCE of an
// already-authorized cancel / no-show transition. The outer SettleDepositOnCancel
// authorizes the real actor for the real trigger ONCE; the inner void / capture
// (captureHold/voidHold never re-check the actor) and the inner pre-order refund
// (RefundUseCase.Settle) then run as the system, exactly as the booking worker
// closes a no-show as the system.
var systemActor = Actor{Role: domain.RoleAdmin}

// NewDepositCancellationUseCase constructs the deposit cancellation settlement.
func NewDepositCancellationUseCase(
	payments domain.PaymentRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	managers managerChecker,
	bookings bookingReader,
	deadline cancelDeadlineResolver,
	refunds RefundUseCase,
	tx domain.TxManager,
) DepositCancellationUseCase {
	return &depositCancellationUseCase{
		payments: payments,
		capvoid:  &captureVoidUseCase{payments: payments, ledger: ledger, outbox: outbox, gateways: gateways, managers: managers, tx: tx},
		refunds:  refunds,
		bookings: bookings,
		managers: managers,
		deadline: deadline,
	}
}

// SettleDepositOnCancel is safe to call for EVERY cancel / no-show transition,
// including bookings that never took a deposit: when the booking has no live
// payment it returns (nil, nil), a cheap no-op, so the caller does not have to
// know in advance whether a deposit exists.
func (u *depositCancellationUseCase) SettleDepositOnCancel(ctx context.Context, actor Actor, bookingID uuid.UUID, in DepositCancelInput) (*domain.Payment, error) {
	if !in.Trigger.Valid() {
		return nil, fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, in.Trigger)
	}

	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// No live payment for this booking — no deposit was taken, or it
			// was already released/forfeited/expired. Nothing to settle.
			return nil, nil
		}
		return nil, err
	}
	if err := authorizeSettle(ctx, u.managers, actor, in.Trigger, p); err != nil {
		return nil, err
	}

	// A PRE-ORDER was captured immediately at payment time (kitchen prepares the
	// food). On cancel it is refunded or kept, never voided/re-captured.
	if p.Purpose == domain.PurposePreorder {
		return u.settlePreorder(ctx, p, in)
	}

	// Idempotent terminal states: the deposit was already settled one way or the
	// other (a double cancel, or a cancel racing the no-show worker). Return it
	// as-is — no money moves a second time.
	if p.Status == domain.PaymentVoided || p.Status == domain.PaymentCaptured {
		return p, nil
	}
	if p.Status != domain.PaymentAuthorized {
		// capturing / voiding: another settlement attempt is mid-flight at the
		// acquirer. Do not start a second one; the CAS in captureHold/voidHold
		// would reject it anyway, but failing fast here is clearer.
		return nil, fmt.Errorf("%w: deposit is %s, another settlement is already in flight", domain.ErrInvalidStatus, p.Status)
	}

	capture, err := u.shouldCapture(ctx, p, in)
	if err != nil {
		return nil, err
	}

	if capture {
		out, err := u.capvoid.captureHold(ctx, p)
		if err != nil {
			return nil, err
		}
		logging.FromContext(ctx).Info("payment.deposit_forfeited",
			slog.String("payment_id", p.ID.String()),
			slog.String("booking_id", p.BookingID.String()),
			slog.String("trigger", string(in.Trigger)))
		return out, nil
	}

	reason := "booking cancelled in time"
	if in.Reason != nil {
		reason = *in.Reason
	}
	out, err := u.capvoid.voidHold(ctx, p, reason)
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Info("payment.deposit_released",
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("trigger", string(in.Trigger)))
	return out, nil
}

// settlePreorder settles an already-captured pre-order on cancel / no-show:
//
//	guest cancels EARLIER than free_cancel_window, or venue cancels  → FULL refund
//	guest cancels LATER than free_cancel_window, or no-show          → keep (food prepped)
//
// Owner-confirmed: an early cancellation returns EVERYTHING to the guest with
// no loss. The full make-whole refund is exactly domain.SettleRefund's
// venue-cancel outcome (guest gets the whole total, service fee included), so
// the refund is settled through the existing RefundUseCase.Settle with
// RefundTriggerVenueCancel regardless of whether the guest or the venue
// initiated it — the money outcome is identical, and a partial (acquiring-
// withheld) refund would violate "no loss". A late cancel / no-show keeps the
// pre-order with the venue: the capture already credited it, nothing moves.
//
// Idempotent: a refunded pre-order is a no-op; a still-captured one resumes the
// SAME refund (deterministic idempotency key + trigger) via Settle's own
// SettledAt / refund-row guards, so a double cancel never refunds twice.
func (u *depositCancellationUseCase) settlePreorder(ctx context.Context, p *domain.Payment, in DepositCancelInput) (*domain.Payment, error) {
	if p.Status == domain.PaymentRefunded || p.Status == domain.PaymentPartiallyRefunded {
		return p, nil // already refunded — nothing more to do
	}
	if p.Status != domain.PaymentCaptured {
		// Still authorized (immediate capture pending/declined) or capturing:
		// the money is not yet the venue's to refund. The reconciler / capture
		// retry resolves it first; a later cancel settlement can then act.
		return nil, fmt.Errorf("%w: pre-order is %s, cannot settle on cancel yet", domain.ErrInvalidStatus, p.Status)
	}

	forfeit, err := u.shouldCapture(ctx, p, in)
	if err != nil {
		return nil, err
	}
	if forfeit {
		// Late cancel / no-show: the food was prepared, the venue keeps the
		// pre-order. The capture already credited it — no money moves.
		logging.FromContext(ctx).Info("payment.preorder_kept",
			slog.String("payment_id", p.ID.String()),
			slog.String("booking_id", p.BookingID.String()),
			slog.String("trigger", string(in.Trigger)))
		return p, nil
	}

	out, err := u.refunds.Settle(ctx, systemActor, p.BookingID, SettleInput{
		Trigger:        domain.RefundTriggerVenueCancel, // full make-whole refund (see doc above)
		IdempotencyKey: p.BookingID.String() + ":preorder-cancel-refund",
		Reason:         in.Reason,
	})
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Info("payment.preorder_refunded",
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("trigger", string(in.Trigger)))
	return out, nil
}

// shouldCapture decides whether the held deposit is forfeited to the venue
// (capture) or released to the guest (void):
//   - a venue-caused cancellation always releases the hold;
//   - a no-show always forfeits it (settled exactly like a late cancel, per the
//     owner-confirmed rule);
//   - a guest cancellation forfeits only when it landed at or after the venue's
//     per-restaurant free-cancellation deadline (starts_at − free_cancel_window),
//     computed server-side through cancelDeadlineResolver — the guest never
//     supplies the deadline.
func (u *depositCancellationUseCase) shouldCapture(ctx context.Context, p *domain.Payment, in DepositCancelInput) (bool, error) {
	switch in.Trigger {
	case domain.RefundTriggerVenueCancel:
		return false, nil
	case domain.RefundTriggerNoShow:
		return true, nil
	case domain.RefundTriggerGuestCancel:
		booking, err := u.bookings.GetByID(ctx, p.BookingID)
		if err != nil {
			return false, err
		}
		deadline, err := u.deadline.CancelDeadlineFor(ctx, *booking)
		if err != nil {
			return false, err
		}
		cancelledAt := time.Now()
		if in.CancelledAt != nil {
			cancelledAt = *in.CancelledAt
		}
		// Late = the cancellation is NOT before the deadline → forfeit. This is
		// the same boundary domain.SettleRefund uses for the captured-refund
		// path, keeping the two settlement flows consistent.
		return !cancelledAt.Before(deadline), nil
	default:
		return false, fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, in.Trigger)
	}
}
