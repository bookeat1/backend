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
	// EventBookingReminder is the pre-visit reminder for the GUEST. Unlike every
	// other event here it does not describe a status change: it is emitted by
	// the booking worker's reminder pass when the visit draws near, in the same
	// transaction that stamps bookings.guest_reminder_sent_at — so it is emitted
	// at most once per booking, restart or no restart.
	EventBookingReminder BookingEventType = "booking.reminder"
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
	// Attempts counts the delivery attempts already made (0 = never tried).
	// The dispatcher derives the backoff and the give-up decision from it.
	Attempts int
	// NextAttemptAt is the earliest moment this event may be claimed again;
	// nil means "due now". A failed event is pushed into the FUTURE instead of
	// staying at the head of the queue, which is what keeps one broken channel
	// from starving every other channel's fresher events.
	NextAttemptAt *time.Time
	// LastError is why the last attempt failed, kept on the row so an abandoned
	// event explains itself long after the log line has rotated away.
	LastError string
	// AbandonedAt is set when the attempt budget ran out. Such a row is the
	// dead letter: never claimed again, and deliberately NOT marked published,
	// so "given up on" stays distinguishable from "delivered".
	AbandonedAt *time.Time
}

// BookingOutboxFailure describes one event's failed delivery attempt. Reschedule
// and Abandon take a batch of them because a whole claim batch can fail against
// the same broken channel in the same tick.
type BookingOutboxFailure struct {
	ID        uuid.UUID
	LastError string
	// NextAttemptAt is when the event becomes claimable again. Ignored by
	// Abandon, which by definition schedules no next attempt.
	NextAttemptAt time.Time
}

// BookingOutboxRepository persists and drains booking events.
type BookingOutboxRepository interface {
	// Create inserts an event; call inside the same TxManager as the mutation.
	Create(ctx context.Context, e *BookingOutboxEvent) error
	// ClaimDue locks up to limit events that are undelivered, not abandoned and
	// due at now, using FOR UPDATE SKIP LOCKED so parallel workers do not
	// collide. Never-attempted events are returned FIRST: a retry may only use
	// the batch capacity a fresh event did not need.
	ClaimDue(ctx context.Context, limit int, now time.Time) ([]BookingOutboxEvent, error)
	// Reschedule records a failed attempt: attempts += 1, the error is stored
	// and the event is moved to the back of the queue until NextAttemptAt.
	Reschedule(ctx context.Context, failures []BookingOutboxFailure) error
	// Abandon records the final failed attempt and stamps abandoned_at: the
	// event stops being claimed and becomes visible as a dead letter. It is
	// NOT published — nothing is ever silently dropped.
	Abandon(ctx context.Context, failures []BookingOutboxFailure, at time.Time) error
	// ExistsForBooking reports whether an event of that type was already
	// recorded for the booking. Used by the background worker to emit
	// at-most-once events (e.g. the confirm-SLA escalation) without adding a
	// flag column to bookings.
	ExistsForBooking(ctx context.Context, bookingID uuid.UUID, eventType BookingEventType) (bool, error)
	MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error
}
