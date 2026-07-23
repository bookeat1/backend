package payments

import (
	"context"
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
	tx domain.TxManager,
) CaptureUseCase {
	return &captureVoidUseCase{payments: payments, ledger: ledger, outbox: outbox, gateways: gateways, tx: tx}
}

// NewVoidUseCase constructs the hold-release usecase.
func NewVoidUseCase(
	payments domain.PaymentRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	tx domain.TxManager,
) VoidUseCase {
	return &captureVoidUseCase{payments: payments, outbox: outbox, gateways: gateways, tx: tx}
}

// CaptureOnSeating captures the booking's live hold in full. It is
// idempotent: calling it again on an already-captured payment is a no-op,
// not an error — staff retrying a slow request must not risk a second
// capture attempt at the acquirer (the sandbox checklist's item #7 leaves
// "does a repeated clearing clear twice?" unconfirmed, so this usecase never
// calls Capture a second time for the same payment; it trusts its own local
// state instead of re-asking the acquirer).
func (u *captureVoidUseCase) CaptureOnSeating(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.Payment, error) {
	if !actor.staff() {
		return nil, fmt.Errorf("%w: only venue staff can capture a hold", domain.ErrForbidden)
	}
	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
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

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		return nil, err
	}

	// External call, deliberately outside any DB transaction.
	if _, err := gw.Capture(ctx, *p.ProviderPaymentID, p.Total()); err != nil {
		return nil, fmt.Errorf("capture with %s: %w", p.Provider, err)
	}

	now := time.Now()
	entries := captureLedgerEntries(*p, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return nil, err
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, now); err != nil {
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

// VoidOnRejection releases the booking's live hold. Idempotent for the same
// reason as CaptureOnSeating: calling it twice on an already-voided payment
// is a no-op.
func (u *captureVoidUseCase) VoidOnRejection(ctx context.Context, actor Actor, bookingID uuid.UUID, reason string) (*domain.Payment, error) {
	if !actor.staff() {
		return nil, fmt.Errorf("%w: only venue staff can release a hold", domain.ErrForbidden)
	}
	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
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

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		return nil, err
	}

	// External call, deliberately outside any DB transaction.
	if err := gw.Void(ctx, *p.ProviderPaymentID); err != nil {
		return nil, fmt.Errorf("void with %s: %w", p.Provider, err)
	}

	now := time.Now()
	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentVoided, now); err != nil {
			return err
		}
		p.Status = domain.PaymentVoided
		p.VoidedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentVoided, now)
	})
	if err != nil {
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
