package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// BookingEventType names a booking event published to the notification layer.
type BookingEventType string

const (
	EventBookingCreated   BookingEventType = "booking.created"
	EventBookingConfirmed BookingEventType = "booking.confirmed"
	EventBookingWaitlist  BookingEventType = "booking.waitlisted"
	EventBookingArrived   BookingEventType = "booking.arrived"
	EventBookingCompleted BookingEventType = "booking.completed"
	EventBookingCancelled BookingEventType = "booking.cancelled"
	EventBookingNoShow    BookingEventType = "booking.no_show"
	EventBookingUpdated   BookingEventType = "booking.updated"
	EventBookingEscalated BookingEventType = "booking.confirm_sla_breached"
	EventBookingMessage   BookingEventType = "booking.message_created"
)

// BookingOutboxEvent is a transactional-outbox row. It is inserted in the same
// transaction as the booking mutation it describes and drained by a worker that
// hands the payload to the existing edge notification layer. PublishedAt nil
// means not yet delivered.
type BookingOutboxEvent struct {
	ID          uuid.UUID
	BookingID   uuid.UUID
	EventType   BookingEventType
	Payload     json.RawMessage
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// BookingOutboxRepository persists and drains booking events.
type BookingOutboxRepository interface {
	// Create inserts an event; call inside the same TxManager as the mutation.
	Create(ctx context.Context, e *BookingOutboxEvent) error
	// ClaimUnpublished locks up to limit undelivered events using FOR UPDATE
	// SKIP LOCKED so parallel workers do not collide.
	ClaimUnpublished(ctx context.Context, limit int) ([]BookingOutboxEvent, error)
	MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error
}
