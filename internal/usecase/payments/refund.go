package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// RefundUseCase resolves a cancelled or a no-show booking's captured payment
// into its final split across guest / restaurant / platform / acquirer
// (spec §9.1, scenarios 5 and 6). It is a ONE-SHOT, whole-payment settlement:
// a payment can be settled exactly once (GetLiveByBookingID stops returning it
// the moment its status leaves authorized/captured). Refunding a single
// pre-ordered line while the rest of the booking stands is a different
// operation and is explicitly out of scope here (spec §6) — see the report
// for the KNOWN GAP this leaves.
type RefundUseCase interface {
	Settle(ctx context.Context, actor Actor, bookingID uuid.UUID, in SettleInput) (*domain.Payment, error)
}

// SettleInput carries what only the caller can know: why the booking ended
// (Trigger), and when (CancelledAt / CancelDeadline — domain.SettleRefund is a
// pure function of these, see its doc comment on reproducibility in a
// dispute).
type SettleInput struct {
	Trigger        domain.RefundTrigger
	CancelledAt    time.Time
	CancelDeadline time.Time
	Reason         *string
	// IdempotencyKey guards the acquirer-facing refund call the same way
	// CreateInput.IdempotencyKey guards Authorize: a retry after a timeout
	// must not refund twice.
	IdempotencyKey string
}

type refundUseCase struct {
	payments domain.PaymentRepository
	refunds  domain.PaymentRefundRepository
	ledger   domain.PaymentLedgerRepository
	outbox   domain.PaymentOutboxRepository
	gateways gatewayResolver
	tx       domain.TxManager
	cfg      Config
}

// NewRefundUseCase constructs the settlement usecase.
func NewRefundUseCase(
	payments domain.PaymentRepository,
	refunds domain.PaymentRefundRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	tx domain.TxManager,
	cfg Config,
) RefundUseCase {
	return &refundUseCase{
		payments: payments, refunds: refunds, ledger: ledger, outbox: outbox,
		gateways: gateways, tx: tx, cfg: cfg.withDefaults(),
	}
}

// Settle computes the settlement, and either:
//   - writes nothing but an audit event when no money moves (a late
//     cancellation or a no-show: the venue already holds what it is entitled
//     to, see settlementLedgerEntries's doc comment on why this batch is
//     legitimately empty), or
//   - refunds the guest's share at the acquirer (external call, OUTSIDE any
//     DB transaction) and only then commits the refund row, the ledger delta
//     and the payment's terminal status together.
func (u *refundUseCase) Settle(ctx context.Context, actor Actor, bookingID uuid.UUID, in SettleInput) (*domain.Payment, error) {
	if !in.Trigger.Valid() {
		return nil, fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, in.Trigger)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}

	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeSettle(actor, in.Trigger, p); err != nil {
		return nil, err
	}
	if p.Status != domain.PaymentCaptured {
		return nil, fmt.Errorf(
			"%w: payment is %s, settle only applies to a captured payment (an authorized hold is voided, not settled)",
			domain.ErrInvalidStatus, p.Status)
	}

	settlement, err := domain.SettleRefund(*p, in.Trigger, in.CancelledAt, in.CancelDeadline,
		domain.RefundPolicy{AcquiringBps: u.cfg.RefundAcquiringBps})
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if settlement.GuestMinor == 0 {
		return u.settleWithNoRefund(ctx, p, settlement, now)
	}
	return u.settleWithRefund(ctx, p, settlement, in, now)
}

// settleWithNoRefund handles the late-cancellation / no-show outcome: the
// venue keeps exactly what capture already booked it, so there is nothing new
// to move. It still records that the booking's fate was resolved, for audit
// and for the (separate, out-of-scope) payout register.
func (u *refundUseCase) settleWithNoRefund(ctx context.Context, p *domain.Payment, settlement domain.Settlement, now time.Time) (*domain.Payment, error) {
	entries := settlementLedgerEntries(*p, settlement, nil, now)
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if len(entries) > 0 {
			// Defensive: the algorithm should never produce a non-empty
			// batch when GuestMinor is zero, but if it ever does, the
			// invariant is still checked before it is written.
			if err := domain.ValidateLedgerBalance(entries); err != nil {
				return err
			}
			if err := u.ledger.CreateBatch(ctx, entries); err != nil {
				return err
			}
		}
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentSettled, now)
	})
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentSettled,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.Int64("restaurant_minor", settlement.RestaurantMinor),
		slog.Int64("platform_minor", settlement.PlatformMinor),
	)
	return p, nil
}

// settleWithRefund handles guest cancellation before the deadline and venue
// cancellation: money goes back to the guest.
//
// Sequencing is deliberate and split into two independently-durable steps,
// NOT one big transaction, because the acquirer call sits between them and a
// DB transaction must never wrap an external call:
//
//  1. call the acquirer, then immediately persist the refund row as
//     succeeded/failed on its OWN write (u.tx.Detach) — this is the durable
//     proof that money moved, and it survives even if step 2 fails;
//  2. only then attempt the ledger delta + the payment's terminal status +
//     the outbox event as one atomic commit.
//
// If step 2 fails after step 1 succeeded, the error says so explicitly
// ("needs reconciliation") instead of silently reporting success — the
// KNOWN GAP is that closing that gap automatically is the reconciliation
// worker's job, not built here (see the report).
func (u *refundUseCase) settleWithRefund(ctx context.Context, p *domain.Payment, settlement domain.Settlement, in SettleInput, now time.Time) (*domain.Payment, error) {
	existing, err := u.refunds.GetByIdempotencyKey(ctx, p.ID, in.IdempotencyKey)
	switch {
	case err == nil:
		// A previous attempt with this key exists — resume it below instead
		// of asking the acquirer for a brand new refund.
	case errors.Is(err, domain.ErrNotFound):
		existing = &domain.PaymentRefund{
			ID: uuid.New(), PaymentID: p.ID, AmountMinor: settlement.GuestMinor, Currency: p.Currency,
			Status: domain.RefundCreated, Reason: in.Reason, IdempotencyKey: in.IdempotencyKey,
			CreatedAt: now, UpdatedAt: now,
		}
		if cerr := u.refunds.Create(u.tx.Detach(ctx), existing); cerr != nil {
			if !errors.Is(cerr, domain.ErrAlreadyExists) {
				return nil, cerr
			}
			// Lost the race to insert: a concurrent identical retry won.
			// Resume ITS row instead of creating a second attempt.
			existing, err = u.refunds.GetByIdempotencyKey(ctx, p.ID, in.IdempotencyKey)
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, err
	}

	if existing.Status != domain.RefundSucceeded {
		gw, err := u.gateways.ForRefund(p.Provider)
		if err != nil {
			return nil, err
		}
		if p.ProviderPaymentID == nil {
			return nil, fmt.Errorf("payment %s has no provider payment id: %w", p.ID, domain.ErrValidation)
		}

		// External call, deliberately outside any DB transaction. The
		// acquirer sees our own idempotency key
		// (idx_payment_refunds_idempotency), so a retry after a timeout
		// resolves to the same refund (spec §8) instead of returning money
		// twice.
		gwResp, err := gw.Refund(ctx, *p.ProviderPaymentID, domain.Money{AmountMinor: settlement.GuestMinor, Currency: p.Currency})
		if err != nil {
			failMsg := err.Error()
			existing.Status = domain.RefundFailed
			existing.FailureMessage = &failMsg
			existing.UpdatedAt = time.Now()
			if perr := u.refunds.Update(u.tx.Detach(ctx), existing); perr != nil {
				logging.FromContext(ctx).Error("payment.refund_attempt_not_recorded",
					slog.String("payment_id", p.ID.String()), slog.String("error", perr.Error()))
			}
			return nil, fmt.Errorf("refund with %s: %w", p.Provider, err)
		}
		existing.Status = domain.RefundSucceeded
		existing.ProviderRefundID = nullableStr(gwResp.ProviderRefundID)
		existing.UpdatedAt = time.Now()
		// Durable BEFORE the ledger/status commit below — this row is the
		// proof money already moved if that commit fails.
		if perr := u.refunds.Update(u.tx.Detach(ctx), existing); perr != nil {
			return nil, fmt.Errorf("refund succeeded at %s but recording it failed, payment %s needs reconciliation: %w",
				p.Provider, p.ID, perr)
		}
	}

	entries := settlementLedgerEntries(*p, settlement, &existing.ID, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return nil, err
	}
	// This usecase always runs a whole-payment, one-shot settlement: a
	// partial pre-order refund is a different, unimplemented operation
	// (spec §6), so the terminal status here is always the full "refunded",
	// never "partially_refunded" — even when the guest is not made whole
	// (early cancellation withholds the acquiring cost): there is no further
	// refund action left to take against this payment.
	newStatus := domain.PaymentRefunded

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, newStatus, now); err != nil {
			return err
		}
		p.Status = newStatus
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentRefunded, now)
	})
	if err != nil {
		return nil, fmt.Errorf("refund succeeded at %s but local ledger/status commit failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, err)
	}

	logging.FromContext(ctx).Info(logging.EventPaymentRefunded,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.Int64("guest_minor", settlement.GuestMinor),
		slog.Int64("acquirer_minor", settlement.AcquirerMinor),
	)
	return p, nil
}

// authorizeSettle decides who may trigger which outcome: a guest may only
// cancel their own booking (or an account-less guest booking, same reasoning
// as authorizeCreate); a venue-caused cancellation or a no-show is a staff
// call only — a guest must never be able to mark themselves a no-show to
// dodge the late-cancellation forfeit.
func authorizeSettle(actor Actor, trigger domain.RefundTrigger, p *domain.Payment) error {
	switch trigger {
	case domain.RefundTriggerGuestCancel:
		if actor.staff() {
			return nil
		}
		if p.UserID != nil && !actor.isUser(p.UserID) {
			return fmt.Errorf("%w: payment belongs to another guest", domain.ErrForbidden)
		}
		return nil
	case domain.RefundTriggerVenueCancel, domain.RefundTriggerNoShow:
		if !actor.staff() {
			return fmt.Errorf("%w: only venue staff can record this outcome", domain.ErrForbidden)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, trigger)
	}
}
