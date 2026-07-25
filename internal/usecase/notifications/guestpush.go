package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// MobilePushVerdict is the provider's answer for ONE device token. It is a small
// enum rather than the HTTP status the web-push / telegram seams return because
// the mobile providers answer per token INSIDE a 200 response (Expo returns
// {"status":"error","details":{"error":"DeviceNotRegistered"}} with HTTP 200),
// so a status code alone cannot tell "delivered" from "this device is dead".
type MobilePushVerdict int

const (
	// MobilePushDelivered — the provider accepted the message.
	MobilePushDelivered MobilePushVerdict = iota
	// MobilePushDeviceGone — the token is dead (app uninstalled, token rotated).
	// Not retryable: the notifier deactivates the token instead.
	MobilePushDeviceGone
	// MobilePushRejected — the provider refused this specific message for a
	// reason retrying cannot fix (malformed token, message too big). Not
	// retryable: logged and given up on, so one bad device never blocks the
	// outbox for everyone else.
	MobilePushRejected
)

// MobilePushSender delivers ONE notification to ONE device token. It is the seam
// that keeps the provider (Expo today, FCM/APNs later) and the network out of
// the notifier, so the gate / fan-out / dedupe behaviour is unit-testable
// without a device. A non-nil error means a TRANSIENT failure (timeout, 429,
// 5xx) the caller retries on the next tick; the verdict is meaningless then and
// must be ignored.
type MobilePushSender func(ctx context.Context, token string, msg MobilePushMessage) (MobilePushVerdict, error)

// MobilePushMessage is the rendered, non-sensitive notification. Data carries
// only ids the app uses to deep-link into the booking screen — never a phone
// number, never a payment detail, never the token itself.
type MobilePushMessage struct {
	Title string
	Body  string
	Data  map[string]string
}

// venueNameReader resolves a venue's display name for the guest-facing text. A
// minimal local port (bound to postgres/notification.Venues in bootstrap) rather
// than the catalog repository: the notifier needs one string per event, not the
// whole restaurant aggregate.
type venueNameReader interface {
	Name(ctx context.Context, restaurantID uuid.UUID) (string, error)
}

// GuestPushNotifier is the GUEST channel: it pushes to the signed-in guest's own
// phones about the guest's OWN booking. It rides the same dispatcher, the same
// booking outbox and the same delivery ledger as the staff channels — there is
// no second delivery mechanism.
//
// Three rules distinguish it from a staff channel:
//
//   - the guest's opt-out is consulted FIRST, on every event, through
//     GuestNotificationGate. An opted-out guest is SKIPPED (nil error, the event
//     is marked processed), never errored — a permanent opt-out that returned an
//     error would jam the outbox forever;
//   - the fan-out set is the booking's own user_id, not a restaurant. A booking
//     without an account (phone / admin-entered) has nobody to notify;
//   - the dedupe target is the device-token row id, so a redelivery caused by a
//     sibling channel's failure never double-pushes the same phone.
//
// When no push provider is configured the notifier is built DISABLED: Notify
// logs once and no-ops, so the dispatcher still drains the outbox and the worker
// never crashes for lack of credentials — the same discipline as web push
// without VAPID keys and Amplitude without an API key.
type GuestPushNotifier struct {
	tokens     domain.DevicePushTokenRepository
	deliveries domain.NotificationDeliveryRepository
	gate       *GuestNotificationGate
	venues     venueNameReader
	send       MobilePushSender
	enabled    bool // a push provider is configured
	log        *slog.Logger
}

// NewGuestPushNotifier builds the guest mobile-push channel. Pass enabled=false
// (or a nil sender) to run it as a clean no-op when no provider is configured.
func NewGuestPushNotifier(
	tokens domain.DevicePushTokenRepository,
	deliveries domain.NotificationDeliveryRepository,
	gate *GuestNotificationGate,
	venues venueNameReader,
	send MobilePushSender,
	enabled bool,
	log *slog.Logger,
) *GuestPushNotifier {
	return &GuestPushNotifier{
		tokens: tokens, deliveries: deliveries, gate: gate, venues: venues,
		send: send, enabled: enabled && send != nil, log: log,
	}
}

var _ Notifier = (*GuestPushNotifier)(nil)

func (g *GuestPushNotifier) Channel() domain.NotificationChannel { return domain.ChannelMobilePush }

// Interested lists the three moments the guest actually needs to hear about,
// matching what the old Supabase edge functions sent:
//
//	booking.confirmed — the venue accepted; this is the one the guest waits for;
//	booking.cancelled — the booking is off (filtered further in Notify);
//	booking.reminder  — the pre-visit nudge from the worker's reminder pass.
//
// booking.created is deliberately NOT here: the guest just tapped "book" and is
// looking at the confirmation screen. no_show / completed / arrived are venue
// bookkeeping the guest gains nothing from.
func (g *GuestPushNotifier) Interested(t domain.BookingEventType) bool {
	switch t {
	case domain.EventBookingConfirmed, domain.EventBookingCancelled, domain.EventBookingReminder:
		return true
	default:
		return false
	}
}

func (g *GuestPushNotifier) Notify(ctx context.Context, e Event) error {
	if !g.enabled {
		g.log.Debug("guest push disabled (no provider configured), skipping",
			slog.String("booking_id", e.BookingID.String()))
		return nil
	}
	if e.GuestUserID == nil {
		// A phone or admin-entered booking: no account, so no device and no
		// stored preference. Nothing to do — and not an error.
		return nil
	}
	// DECISION (owner may revisit): a cancellation the GUEST performed
	// themselves is not echoed back at them. They tapped "cancel" in the app a
	// second ago and are looking at the result; a push saying what they just did
	// reads as a bug. A venue-side or system cancellation IS pushed — that one
	// is news the guest has no other way of learning.
	if e.Type == domain.EventBookingCancelled && e.CancelledBy == domain.CancelledByGuest {
		return nil
	}

	// The opt-out gate, before anything is read or sent. A read error is
	// returned (not treated as "allowed"): the event stays unpublished and is
	// retried, which is strictly better than pushing to a guest who may have
	// opted out.
	allowed, err := g.gate.Allows(ctx, *e.GuestUserID, domain.ChannelMobilePush)
	if err != nil {
		return fmt.Errorf("guest push: %w", err)
	}
	if !allowed {
		g.log.Debug("guest opted out of push, skipping",
			slog.String("booking_id", e.BookingID.String()))
		return nil
	}

	tokens, err := g.tokens.ListActiveByUser(ctx, *e.GuestUserID)
	if err != nil {
		return fmt.Errorf("guest push: list device tokens: %w", err)
	}
	if len(tokens) == 0 {
		return nil
	}

	venue, err := g.venues.Name(ctx, e.RestaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The venue vanished from the catalog. The message degrades to a
			// nameless one rather than blocking the event forever.
			venue = ""
		} else {
			return fmt.Errorf("guest push: read venue name: %w", err)
		}
	}
	msg, ok := buildGuestMessage(e, venue)
	if !ok {
		// An event type Interested claims but buildGuestMessage has no text for
		// — a programming error, not a delivery failure. Drain it rather than
		// retry it forever.
		g.log.Error("guest push: no message template for event, skipping",
			slog.String("event_type", string(e.Type)))
		return nil
	}

	var firstErr error
	for _, t := range tokens {
		// Dedupe: a redelivery (the event stayed unpublished because a sibling
		// channel failed) must not re-push a device that already got it.
		already, err := g.deliveries.AlreadyDelivered(ctx, e.OutboxEventID, domain.ChannelMobilePush, t.ID)
		if err != nil {
			firstErr = errOr(firstErr, fmt.Errorf("guest push: check delivery: %w", err))
			continue
		}
		if already {
			continue
		}

		verdict, err := g.send(ctx, t.Token, msg)
		if err != nil {
			// Transport / 429 / 5xx — retryable. The token itself is NEVER put
			// in the error: it is a device credential (see the repo's masking
			// discipline for phones), so the row id identifies the device.
			firstErr = errOr(firstErr, fmt.Errorf("guest push: send to device %s: %w", t.ID, err))
			continue
		}
		switch verdict {
		case MobilePushDelivered:
			// Record AFTER success (at-least-once: a crash here re-sends next
			// tick, never drops the notification).
			if err := g.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelMobilePush, t.ID); err != nil {
				firstErr = errOr(firstErr, fmt.Errorf("guest push: record delivery: %w", err))
			}
		case MobilePushDeviceGone:
			g.log.Info("guest push: device token gone, deactivating",
				slog.String("device_token_id", t.ID.String()))
			if err := g.tokens.DeactivateByID(ctx, t.ID); err != nil {
				g.log.Error("guest push: deactivate gone device token failed",
					slog.String("device_token_id", t.ID.String()), slog.String("error", err.Error()))
			}
		default:
			// Rejected: retrying cannot succeed. Log and let the event drain.
			g.log.Warn("guest push: provider rejected the message, giving up on this device",
				slog.String("device_token_id", t.ID.String()))
		}
	}
	return firstErr
}

// buildGuestMessage renders the Russian guest-facing text. It carries ONLY what
// the guest already knows about their own booking — venue, date/time, party
// size. No phone, no payment data, no token. Returns ok=false for an event type
// it has no template for.
//
// Times are rendered in the process's local zone, the same convention the staff
// channels use (see buildTelegramText). The venue's own timezone would be more
// correct for a guest travelling abroad — deliberately deferred rather than
// half-solved here, since it would need the venue policy in the notifier.
func buildGuestMessage(e Event, venue string) (MobilePushMessage, bool) {
	when := e.StartsAt.Local().Format("02.01 в 15:04")
	at := ""
	if venue != "" {
		at = "«" + venue + "» · "
	}
	var title, body string
	switch e.Type {
	case domain.EventBookingConfirmed:
		title = "Бронь подтверждена"
		body = fmt.Sprintf("%s%s · %d чел.", at, when, e.Guests)
	case domain.EventBookingCancelled:
		title = "Бронь отменена"
		body = fmt.Sprintf("%s%s · %d чел.", at, when, e.Guests)
	case domain.EventBookingReminder:
		title = "Напоминание о брони"
		body = fmt.Sprintf("%s%s · %d чел.", at, when, e.Guests)
	default:
		return MobilePushMessage{}, false
	}
	return MobilePushMessage{
		Title: title,
		Body:  body,
		Data: map[string]string{
			"event":         string(e.Type),
			"booking_id":    e.BookingID.String(),
			"restaurant_id": e.RestaurantID.String(),
			"starts_at":     e.StartsAt.Format(time.RFC3339),
		},
	}, true
}
