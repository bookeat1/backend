package bookings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
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
	if err := publish(ctx, outbox, b, eventForStatus(b.Status), at); err != nil {
		return err
	}
	logTransition(ctx, b, from, actorType)
	return nil
}

// logTransition writes the single business-event log line for a status
// transition, named by logging.EventBooking* so it is greppable/alertable on
// an exact string. It runs after the transaction's own work has succeeded
// (recordTransition returns nil), so a rolled-back attempt never gets logged
// as if it happened.
func logTransition(ctx context.Context, b *domain.Booking, from *domain.BookingStatus, actorType domain.ActorType) {
	fields := []any{
		slog.String("booking_id", b.ID.String()),
		slog.String("restaurant_id", b.RestaurantID.String()),
		slog.String("to_status", string(b.Status)),
		slog.String("actor_type", string(actorType)),
	}
	if from == nil {
		// The booking's very first row: log as a creation event, with the
		// masked contact so support can find "which booking" from a guest's
		// phone/email without ever seeing the raw value in the log stream.
		fields = append(fields,
			slog.String("phone_masked", logging.MaskPhone(b.PhoneNormalized)),
			slog.Int("guests", b.Guests),
		)
		if b.Email != "" {
			fields = append(fields, slog.String("email_masked", logging.MaskEmail(b.Email)))
		}
		logging.FromContext(ctx).Info(logging.EventBookingCreated, fields...)
		return
	}
	fields = append(fields, slog.String("from_status", string(*from)))
	logging.FromContext(ctx).Info(logging.EventBookingStatusChanged, fields...)

	switch b.Status {
	case domain.BookingCancelled:
		logging.FromContext(ctx).Info(logging.EventBookingCancelled, fields...)
	case domain.BookingNoShow:
		logging.FromContext(ctx).Info(logging.EventBookingNoShow, fields...)
	}
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
