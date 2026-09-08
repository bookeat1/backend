package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// VenueDecision is a venue answering a booking request from a channel that has
// no signed-in user behind it — today, a button in the venue's Telegram alert.
//
// Every other transition in this package authorises a PERSON: it asks who the
// actor is and whether they manage this restaurant. A Telegram button press
// carries no account and no token; the only thing it proves is which chat it
// came from. So the authority here is the chat itself: the caller resolves it
// to a restaurant through the venue's own notification settings, and this
// method's job is to make sure the booking really belongs to that restaurant
// before it changes anything.
//
// This mirrors what the confirm-SLA worker already does — change a booking with
// no user in sight — rather than inventing a fake Actor to satisfy a check that
// was designed for people.
type VenueDecision string

const (
	// VenueDecisionConfirm accepts the request.
	VenueDecisionConfirm VenueDecision = "confirm"
	// VenueDecisionReject refuses it. The booking ends cancelled, attributed to
	// the restaurant, exactly like a rejection from the panel.
	VenueDecisionReject VenueDecision = "reject"
)

// VenueDecisionResult reports what happened, so the caller can tell the venue
// the truth instead of a generic "done".
type VenueDecisionResult struct {
	Booking *domain.Booking
	// Applied is false when the booking was ALREADY in the target state — a
	// double tap on the same button, or a colleague who answered first. Not an
	// error: the venue's intent is satisfied either way, and the caller should
	// say "already confirmed" rather than fail.
	Applied bool
	// Conflict is set when the booking had moved somewhere the decision can no
	// longer be applied from — the guest cancelled it, or it is already
	// finished. The caller reports this instead of pretending to have acted.
	Conflict bool
}

// DecideAsVenue applies a venue's decision to one of its own bookings.
//
// restaurantID is NOT taken from the request: the caller derives it from the
// authenticated channel (the chat registered by that venue). A booking from
// another restaurant is ErrNotFound, not ErrForbidden — an outsider must not
// learn that a booking id exists at all.
func (u *statusUseCase) DecideAsVenue(
	ctx context.Context,
	restaurantID, bookingID uuid.UUID,
	decision VenueDecision,
) (VenueDecisionResult, error) {
	to := domain.BookingConfirmed
	reason := "подтверждено заведением из телеграма"
	if decision == VenueDecisionReject {
		to = domain.BookingCancelled
		reason = "отклонено заведением из телеграма"
	}

	b, err := u.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return VenueDecisionResult{}, err
	}
	// The tenant guard. Deliberately ErrNotFound: a chat that is not this
	// venue's must not be able to probe which booking ids exist.
	if b.RestaurantID != restaurantID {
		return VenueDecisionResult{}, fmt.Errorf("%w: booking", domain.ErrNotFound)
	}
	if b.Status == to {
		return VenueDecisionResult{Booking: b, Applied: false}, nil
	}
	if err := domain.ValidateTransition(b.Status, to); err != nil {
		return VenueDecisionResult{Booking: b, Conflict: true}, nil
	}

	from := b.Status
	at := time.Now()
	b.Status = to
	if to == domain.BookingConfirmed {
		b.ConfirmedAt = &at
	}
	if to == domain.BookingCancelled {
		b.CancelledAt = &at
		by := domain.CancelledByRestaurant
		b.CancelledBy = &by
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if to == domain.BookingCancelled {
			// Cancellation metadata lives on the row, so it has to be written
			// before the status: Update never touches status, and the trigger
			// that frees the table fires on the status write.
			if err := u.bookings.Update(ctx, b); err != nil {
				return err
			}
		}
		if err := u.bookings.UpdateStatus(ctx, b.ID, to, at); err != nil {
			return err
		}
		return recordTransition(ctx, u.history, u.outbox, b, &from,
			domain.ActorManager, nil, &reason, at)
	})
	if err != nil {
		return VenueDecisionResult{}, err
	}
	return VenueDecisionResult{Booking: b, Applied: true}, nil
}
