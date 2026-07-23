package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// StatusUseCase drives the booking state machine (spec §5). Every transition
// checks, in this order: the actor's relation to the booking, whether the actor
// is allowed to make that particular transition, and whether the transition
// itself is legal. The bookings UPDATE, the history row and the outbox event
// always happen inside one transaction.
type StatusUseCase interface {
	Confirm(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
	Reject(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
	Arrive(ctx context.Context, actor Actor, id uuid.UUID) (*domain.Booking, error)
	Complete(ctx context.Context, actor Actor, id uuid.UUID) (*domain.Booking, error)
	NoShow(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
	Cancel(ctx context.Context, actor Actor, id uuid.UUID, in CancelInput) (*domain.Booking, error)
	Waitlist(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
}

// CancelInput carries the cancellation metadata stored on the booking.
type CancelInput struct {
	ReasonCode *string
	Reason     *string
}

type statusUseCase struct {
	bookings    domain.BookingRepository
	history     domain.BookingStatusHistoryRepository
	outbox      domain.BookingOutboxRepository
	restaurants restaurantReader
	managers    managerChecker
	tx          domain.TxManager
	cfg         Config
}

// NewStatusUseCase constructs the booking status usecase.
func NewStatusUseCase(
	bookings domain.BookingRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	restaurants restaurantReader,
	managers managerChecker,
	tx domain.TxManager,
	cfg Config,
) StatusUseCase {
	return &statusUseCase{
		bookings: bookings, history: history, outbox: outbox,
		restaurants: restaurants, managers: managers, tx: tx, cfg: cfg.withDefaults(),
	}
}

func (u *statusUseCase) Confirm(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingConfirmed, reason, false, nil)
}

// Reject is the venue's refusal: the booking ends up cancelled, attributed to
// the restaurant.
func (u *statusUseCase) Reject(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error) {
	// staffOnly: Reject lands on the same status as Cancel, but it is the
	// venue's refusal — a guest must not be able to reach it (it would bypass
	// the cancellation deadline and be attributed to the restaurant).
	return u.transition(ctx, actor, id, domain.BookingCancelled, reason, true, func(b *domain.Booking, acc access, at time.Time) {
		by := domain.CancelledByRestaurant
		b.CancelledBy = &by
		b.CancelledAt = &at
		b.CancellationReason = reason
	})
}

func (u *statusUseCase) Arrive(ctx context.Context, actor Actor, id uuid.UUID) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingArrived, nil, false, nil)
}

func (u *statusUseCase) Complete(ctx context.Context, actor Actor, id uuid.UUID) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingCompleted, nil, false, nil)
}

func (u *statusUseCase) NoShow(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingNoShow, reason, false, nil)
}

func (u *statusUseCase) Waitlist(ctx context.Context, actor Actor, id uuid.UUID, reason *string) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingWaitlist, reason, false, nil)
}

// Cancel is the only transition a guest may perform, and only before
// starts_at - policy.CancelDeadline. Past the deadline only the venue can
// cancel (spec §5).
func (u *statusUseCase) Cancel(ctx context.Context, actor Actor, id uuid.UUID, in CancelInput) (*domain.Booking, error) {
	return u.transition(ctx, actor, id, domain.BookingCancelled, in.Reason, false, func(b *domain.Booking, acc access, at time.Time) {
		by := domain.CancelledByGuest
		if acc.staff() {
			by = domain.CancelledByRestaurant
		}
		b.CancelledBy = &by
		b.CancelledAt = &at
		b.CancellationReasonCode = in.ReasonCode
		b.CancellationReason = in.Reason
	})
}

// transition is the single path through which a booking changes status.
func (u *statusUseCase) transition(
	ctx context.Context,
	actor Actor,
	id uuid.UUID,
	to domain.BookingStatus,
	reason *string,
	staffOnly bool,
	apply func(b *domain.Booking, acc access, at time.Time),
) (*domain.Booking, error) {
	b, err := u.bookings.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	acc, err := authorize(ctx, u.managers, actor, b)
	if err != nil {
		return nil, err
	}
	if err := u.authorizeTransition(ctx, acc, b, to, staffOnly); err != nil {
		return nil, err
	}
	if err := domain.ValidateTransition(b.Status, to); err != nil {
		return nil, fmt.Errorf("%s → %s: %w", b.Status, to, err)
	}

	from := b.Status
	at := time.Now()
	b.Status = to
	switch to {
	case domain.BookingConfirmed:
		b.ConfirmedAt = &at
	case domain.BookingArrived:
		b.ArrivedAt = &at
	}
	if apply != nil {
		apply(b, acc, at)
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Metadata first, status last: Update never writes status, and the DB
		// trigger that syncs booking_tables.active fires on the status write.
		if apply != nil {
			if err := u.bookings.Update(ctx, b); err != nil {
				return err
			}
		}
		if err := u.bookings.UpdateStatus(ctx, b.ID, to, at); err != nil {
			return err
		}
		return recordTransition(ctx, u.history, u.outbox, b, &from, acc.actorType(), actorID(actor), reason, at)
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// authorizeTransition enforces who may request which transition. Staff may make
// any legal transition; a guest may only cancel their own booking, and only
// before the venue's cancellation deadline.
func (u *statusUseCase) authorizeTransition(ctx context.Context, acc access, b *domain.Booking, to domain.BookingStatus, staffOnly bool) error {
	if acc.staff() {
		return nil
	}
	if staffOnly {
		return fmt.Errorf("%w: this action is restricted to venue staff", domain.ErrForbidden)
	}
	if to != domain.BookingCancelled {
		return fmt.Errorf("%w: only the restaurant can set status %s", domain.ErrForbidden, to)
	}
	deadline, err := u.cancelDeadline(ctx, b)
	if err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return fmt.Errorf("%w: free cancellation ended at %s, contact the restaurant",
			domain.ErrForbidden, deadline.UTC().Format(time.RFC3339))
	}
	return nil
}

// cancelDeadline is starts_at minus the venue's cancellation window.
func (u *statusUseCase) cancelDeadline(ctx context.Context, b *domain.Booking) (time.Time, error) {
	rest, err := u.restaurants.GetByID(ctx, b.RestaurantID)
	if err != nil {
		return time.Time{}, err
	}
	return CancelDeadlineFor(rest.Restaurant, u.cfg, b.StartsAt), nil
}
