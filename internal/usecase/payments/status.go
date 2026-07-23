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
	managers managerChecker
}

// NewStatusUseCase constructs the read-only payment status usecase.
func NewStatusUseCase(payments domain.PaymentRepository, managers managerChecker) StatusUseCase {
	return &statusUseCase{payments: payments, managers: managers}
}

// Get reads one payment by its own id.
func (u *statusUseCase) Get(ctx context.Context, actor Actor, paymentID uuid.UUID) (*domain.Payment, error) {
	p, err := u.payments.GetByID(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	if err := authorizeRead(ctx, u.managers, actor, p); err != nil {
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
	if err := authorizeRead(ctx, u.managers, actor, p); err != nil {
		return nil, err
	}
	return p, nil
}

// authorizeRead allows venue staff/admin to see any payment belonging to
// THEIR OWN restaurant (report item #13 — the same tenant scoping every
// money-moving action gets, applied here too so a manager cannot browse
// another venue's payment by guessing a booking id), the payment's own owner
// to see theirs, and — for a guest checkout with no account — an
// unauthenticated actor to see it too, same reasoning as authorizeCreate: the
// payment id itself is the thing the transport layer only ever hands to the
// guest who is paying it.
func authorizeRead(ctx context.Context, managers managerChecker, actor Actor, p *domain.Payment) error {
	if actor.staff() {
		return authorizeStaffForRestaurant(ctx, managers, actor, p.RestaurantID)
	}
	if p.UserID == nil {
		return nil
	}
	if !actor.isUser(p.UserID) {
		return fmt.Errorf("%w: payment", domain.ErrNotFound)
	}
	return nil
}
