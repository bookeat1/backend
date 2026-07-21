package bookings

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// eventForStatus maps a booking status to the outbox event announcing it.
func eventForStatus(s domain.BookingStatus) domain.BookingEventType {
	switch s {
	case domain.BookingConfirmed:
		return domain.EventBookingConfirmed
	case domain.BookingWaitlist:
		return domain.EventBookingWaitlist
	case domain.BookingArrived:
		return domain.EventBookingArrived
	case domain.BookingCompleted:
		return domain.EventBookingCompleted
	case domain.BookingCancelled:
		return domain.EventBookingCancelled
	case domain.BookingNoShow:
		return domain.EventBookingNoShow
	case domain.BookingPending:
		return domain.EventBookingCreated
	default:
		return domain.EventBookingUpdated
	}
}

// recordTransition writes the audit-trail row and the outbox event for a status
// change. It is a free function over the two repositories it needs so both the
// creation usecase and the status usecase can call it without owning each
// other's state. Must run inside the same TxManager transaction as the
// bookings mutation it describes (spec §5).
func recordTransition(
	ctx context.Context,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	b *domain.Booking,
	from *domain.BookingStatus,
	actorType domain.ActorType,
	actorID *uuid.UUID,
	reason *string,
	at time.Time,
) error {
	if err := history.Create(ctx, &domain.BookingStatusChange{
		ID:         uuid.New(),
		BookingID:  b.ID,
		FromStatus: from,
		ToStatus:   b.Status,
		ActorType:  actorType,
		ActorID:    actorID,
		Reason:     reason,
		CreatedAt:  at,
	}); err != nil {
		return err
	}
	return publish(ctx, outbox, b, eventForStatus(b.Status), at)
}

// publish inserts one outbox event describing the booking's current state.
func publish(
	ctx context.Context,
	outbox domain.BookingOutboxRepository,
	b *domain.Booking,
	eventType domain.BookingEventType,
	at time.Time,
) error {
	payload, err := json.Marshal(newBookingPayload(b))
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	return outbox.Create(ctx, &domain.BookingOutboxEvent{
		ID:        uuid.New(),
		BookingID: b.ID,
		EventType: eventType,
		Payload:   payload,
		CreatedAt: at,
	})
}

// bookingPayload is the notification-layer view of a booking. It carries only
// what the edge delivery layer needs to render a message; it is deliberately
// not the full row.
type bookingPayload struct {
	ID           uuid.UUID            `json:"id"`
	RestaurantID uuid.UUID            `json:"restaurant_id"`
	UserID       *uuid.UUID           `json:"user_id,omitempty"`
	Name         string               `json:"name"`
	Phone        string               `json:"phone"`
	Email        string               `json:"email,omitempty"`
	Guests       int                  `json:"guests"`
	StartsAt     time.Time            `json:"starts_at"`
	EndsAt       time.Time            `json:"ends_at"`
	Status       domain.BookingStatus `json:"status"`
	Source       domain.BookingSource `json:"source"`
}

func newBookingPayload(b *domain.Booking) bookingPayload {
	return bookingPayload{
		ID: b.ID, RestaurantID: b.RestaurantID, UserID: b.UserID, Name: b.Name,
		Phone: b.PhoneNormalized, Email: b.Email, Guests: b.Guests,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, Status: b.Status, Source: b.Source,
	}
}
