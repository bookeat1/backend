// Package notifications is the reusable notification backbone: a dispatcher
// drains the booking transactional outbox and fans each event out to the
// registered channel notifiers. Increment 1 ships exactly one channel, web
// push; the Notifier port is the seam future channels (Telegram / CRM / Google
// Calendar) plug into without the dispatcher changing.
package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Event is the channel-agnostic view of a booking outbox row handed to a
// Notifier. It carries the outbox event id (the dedupe key), the booking's
// restaurant (the fan-out scope) and only the few fields a channel needs to
// render a message — never payment secrets or OTP codes.
type Event struct {
	OutboxEventID uuid.UUID
	BookingID     uuid.UUID
	RestaurantID  uuid.UUID
	Type          domain.BookingEventType
	GuestName     string
	Guests        int
	StartsAt      time.Time
}

// Notifier is one outbound channel. Notify MUST be idempotent under redelivery
// (the same Event may arrive again if a sibling channel's send failed and left
// the outbox row unpublished): return nil only when the channel has durably
// handled the event, and a non-nil error to have the dispatcher leave the event
// unpublished for retry.
type Notifier interface {
	// Channel names the channel (for logs and the delivery ledger).
	Channel() domain.NotificationChannel
	// Interested reports whether this channel reacts to an event type. The
	// web-push channel reacts only to booking.created in increment 1.
	Interested(t domain.BookingEventType) bool
	Notify(ctx context.Context, e Event) error
}

// outboxPayload mirrors the subset of bookings.bookingPayload the notification
// layer needs. It is decoded from the outbox row's JSON payload — the payload
// is the contract between the booking usecase (producer) and this dispatcher
// (consumer), so only additive changes are safe on either side.
type outboxPayload struct {
	RestaurantID uuid.UUID `json:"restaurant_id"`
	Name         string    `json:"name"`
	Guests       int       `json:"guests"`
	StartsAt     time.Time `json:"starts_at"`
}

// toEvent decodes an outbox row into the channel-agnostic Event.
func toEvent(row domain.BookingOutboxEvent) (Event, error) {
	var p outboxPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return Event{}, fmt.Errorf("decode outbox payload: %w", err)
	}
	return Event{
		OutboxEventID: row.ID,
		BookingID:     row.BookingID,
		RestaurantID:  p.RestaurantID,
		Type:          row.EventType,
		GuestName:     p.Name,
		Guests:        p.Guests,
		StartsAt:      p.StartsAt,
	}, nil
}
