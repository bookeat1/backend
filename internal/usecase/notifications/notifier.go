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
	// GuestPhone is the number staff call when they need the guest: to confirm
	// a large party, to warn about a delay, to find someone who has not shown
	// up. It has always been in the outbox payload and was simply not decoded,
	// so the venue got an alert it could not act on without opening the panel.
	//
	// STAFF CHANNELS ONLY. It is the one piece of personal data in this Event,
	// and it goes to the venue that is already hosting this guest — never to a
	// guest-facing channel.
	GuestPhone string
	Guests     int
	StartsAt   time.Time
	// GuestUserID is the booking's account owner, nil for a booking made without
	// an account (phone / admin-entered). Guest-facing channels need it to find
	// the devices to notify and to consult the guest's opt-out; staff channels
	// ignore it.
	GuestUserID *uuid.UUID
	// CancelledBy says who cancelled, and is empty on every other event type.
	// The guest channel uses it to avoid echoing a cancellation the guest
	// themselves just performed in the app.
	CancelledBy domain.CancelledBy
	// CancellationReasonCode is the machine-readable system-cancellation reason
	// (domain.CancelReason*), so the guest push can render distinct copy for
	// "you never paid" / "the venue never answered" / "the charge failed" /
	// "the bank released the hold" instead of one generic message for all four.
	// Empty on a venue/guest cancellation and on every non-cancel event.
	CancellationReasonCode *string
	// HoldAmountMinor / HoldCurrency and VenueAnswerDeadlineAt carry the
	// pre-order hold's money and answer deadline (spec
	// preorder-hold-capture-on-confirm-20260924 §criterion 25) — nil/zero on
	// every event except a booking.created release with a live hold that still
	// needs a venue answer. A staff channel uses them to add the "заблокировано
	// N ₸, ответьте до…" line; a channel that ignores them (web push, WhatsApp's
	// fixed approved template) is unaffected.
	HoldAmountMinor       *int64
	HoldCurrency          domain.Currency
	VenueAnswerDeadlineAt *time.Time
	// ReleasedToVenueAt is nil when the booking that generated this event was
	// still hidden behind an unpaid pre-order (never shown to the venue). A
	// staff channel (Telegram) must not announce a cancellation for such a
	// booking — see TelegramNotifier.Notify.
	ReleasedToVenueAt *time.Time
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
	RestaurantID           uuid.UUID          `json:"restaurant_id"`
	UserID                 *uuid.UUID         `json:"user_id,omitempty"`
	Name                   string             `json:"name"`
	Phone                  string             `json:"phone"`
	Guests                 int                `json:"guests"`
	StartsAt               time.Time          `json:"starts_at"`
	CancelledBy            domain.CancelledBy `json:"cancelled_by,omitempty"`
	CancellationReasonCode *string            `json:"cancellation_reason_code,omitempty"`
	HoldAmountMinor        *int64             `json:"hold_amount_minor,omitempty"`
	HoldCurrency           domain.Currency    `json:"hold_currency,omitempty"`
	VenueAnswerDeadlineAt  *time.Time         `json:"venue_answer_deadline_at,omitempty"`
	ReleasedToVenueAt      *time.Time         `json:"released_to_venue_at,omitempty"`
}

// toEvent decodes an outbox row into the channel-agnostic Event.
func toEvent(row domain.BookingOutboxEvent) (Event, error) {
	var p outboxPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return Event{}, fmt.Errorf("decode outbox payload: %w", err)
	}
	return Event{
		OutboxEventID:          row.ID,
		BookingID:              row.BookingID,
		RestaurantID:           p.RestaurantID,
		Type:                   row.EventType,
		GuestName:              p.Name,
		GuestPhone:             p.Phone,
		Guests:                 p.Guests,
		StartsAt:               p.StartsAt,
		GuestUserID:            p.UserID,
		CancelledBy:            p.CancelledBy,
		CancellationReasonCode: p.CancellationReasonCode,
		HoldAmountMinor:        p.HoldAmountMinor,
		HoldCurrency:           p.HoldCurrency,
		VenueAnswerDeadlineAt:  p.VenueAnswerDeadlineAt,
		ReleasedToVenueAt:      p.ReleasedToVenueAt,
	}, nil
}
