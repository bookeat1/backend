package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// StatusUseCase is the read side: a guest checking whether their payment went
// through, and a venue checking the state of a booking's payment
// (spec §6, scenario 7). It never mutates anything.
type StatusUseCase interface {
	Get(ctx context.Context, actor Actor, paymentID uuid.UUID) (*domain.Payment, error)
	GetForBooking(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.Payment, error)
}

type statusUseCase struct {
	payments domain.PaymentRepository
}

// NewStatusUseCase constructs the read-only payment status usecase.
func NewStatusUseCase(payments domain.PaymentRepository) StatusUseCase {
	return &statusUseCase{payments: payments}
}

// Get reads one payment by its own id.
func (u *statusUseCase) Get(ctx context.Context, actor Actor, paymentID uuid.UUID) (*domain.Payment, error) {
	p, err := u.payments.GetByID(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	if err := authorizeRead(actor, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetForBooking reads the booking's current LIVE payment (authorized or
// captured). A guest polling a checkout page and a venue checking whether a
// deposit landed both want "the one that matters right now", not a full
// history — StatusUseCase does not expose List; a restaurant-facing ledger
// / reporting view is a separate, unbuilt concern (KNOWN GAP, see report).
func (u *statusUseCase) GetForBooking(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.Payment, error) {
	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeRead(actor, p); err != nil {
		return nil, err
	}
	return p, nil
}

// authorizeRead allows venue staff/admin to see any payment, the payment's
// own owner to see theirs, and — for a guest checkout with no account — an
// unauthenticated actor to see it too, same reasoning as authorizeCreate: the
// payment id itself is the thing the transport layer only ever hands to the
// guest who is paying it.
func authorizeRead(actor Actor, p *domain.Payment) error {
	if actor.staff() {
		return nil
	}
	if p.UserID == nil {
		return nil
	}
	if !actor.isUser(p.UserID) {
		return fmt.Errorf("%w: payment", domain.ErrNotFound)
	}
	return nil
}
