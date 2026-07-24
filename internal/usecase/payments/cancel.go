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
	bookings bookingReader
	managers managerChecker
	deadline cancelDeadlineResolver
}

// NewDepositCancellationUseCase constructs the deposit cancellation settlement.
func NewDepositCancellationUseCase(
	payments domain.PaymentRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	managers managerChecker,
	bookings bookingReader,
	deadline cancelDeadlineResolver,
	tx domain.TxManager,
) DepositCancellationUseCase {
	return &depositCancellationUseCase{
		payments: payments,
		capvoid:  &captureVoidUseCase{payments: payments, ledger: ledger, outbox: outbox, gateways: gateways, managers: managers, tx: tx},
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

	// A PRE-ORDER is captured immediately at payment time (the kitchen prepares
	// the food), so it is never a hold this path can void or re-capture. What
	// happens to a pre-order on an EARLY cancellation is a DELIBERATELY OPEN
	// question the owner has not answered (see the PR description): the food may
	// already be in preparation, so an automatic refund is NOT assumed here.
	// This path leaves the pre-order untouched.
	if p.Purpose == domain.PurposePreorder {
		// TODO(owner-decision): does an EARLY cancel refund a captured pre-order?
		// Do NOT auto-refund without an explicit instruction. See PR body.
		return p, nil
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
