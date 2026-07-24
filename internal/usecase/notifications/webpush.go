package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// PushSender delivers one encrypted Web Push message to a subscription's
// endpoint and reports the push service's HTTP status. It is the seam that
// keeps the webpush library (and the network) out of the notifier's logic, so
// the fan-out / dedupe / tenant-scoping behaviour is unit-testable without a
// browser or a push service. statusCode is the push service response code
// (201/200 = accepted; 404/410 = the subscription is gone).
type PushSender func(ctx context.Context, sub domain.PushSubscription, payload []byte) (statusCode int, err error)

// WebPushNotifier is the web-push channel: on a new booking it pushes to every
// subscription of THAT booking's restaurant (its owner / manager / hostess
// devices), and nobody else's — the fan-out is scoped by restaurant_id, so
// there is no cross-tenant push.
//
// When VAPID keys are absent the notifier is DISABLED: Notify logs once and
// no-ops, returning nil, so the dispatcher marks the event processed and the
// worker never crashes for lack of keys (the owner enables them later).
type WebPushNotifier struct {
	subs       domain.PushSubscriptionRepository
	deliveries domain.NotificationDeliveryRepository
	settings   domain.RestaurantNotificationSettingsRepository
	send       PushSender
	enabled    bool // VAPID keys configured
	log        *slog.Logger
}

// NewWebPushNotifier builds the web-push channel. Pass enabled=false (or a nil
// sender) to run it as a clean no-op when VAPID keys are not configured.
func NewWebPushNotifier(
	subs domain.PushSubscriptionRepository,
	deliveries domain.NotificationDeliveryRepository,
	settings domain.RestaurantNotificationSettingsRepository,
	send PushSender,
	enabled bool,
	log *slog.Logger,
) *WebPushNotifier {
	return &WebPushNotifier{
		subs: subs, deliveries: deliveries, settings: settings,
		send: send, enabled: enabled && send != nil, log: log,
	}
}

var _ Notifier = (*WebPushNotifier)(nil)

func (w *WebPushNotifier) Channel() domain.NotificationChannel { return domain.ChannelWebPush }

// Interested: increment 1 alerts staff only on a NEW booking.
func (w *WebPushNotifier) Interested(t domain.BookingEventType) bool {
	return t == domain.EventBookingCreated
}

// pushPayload is the minimal, non-sensitive notification body the service
// worker renders. No OTP, no payment data — only what staff already see in the
// venue cabinet: that a booking came in, for when, for how many, under what name.
type pushPayload struct {
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Event        string    `json:"event"`
	BookingID    uuid.UUID `json:"booking_id"`
	RestaurantID uuid.UUID `json:"restaurant_id"`
	Guests       int       `json:"guests"`
	StartsAt     time.Time `json:"starts_at"`
}

func (w *WebPushNotifier) Notify(ctx context.Context, e Event) error {
	if !w.enabled {
		w.log.Debug("web push disabled (no VAPID keys), skipping",
			slog.String("booking_id", e.BookingID.String()))
		return nil
	}

	enabled, err := w.settings.WebPushEnabled(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("web push: read settings: %w", err)
	}
	if !enabled {
		w.log.Debug("web push disabled for restaurant, skipping",
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	subs, err := w.subs.ListByRestaurant(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("web push: list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	payload, err := json.Marshal(buildPayload(e))
	if err != nil {
		return fmt.Errorf("web push: marshal payload: %w", err)
	}

	var firstErr error
	for _, sub := range subs {
		// Dedupe: a redelivery (the event stayed unpublished because a sibling
		// subscription failed) must not re-notify a subscription that already
		// got it.
		already, err := w.deliveries.AlreadyDelivered(ctx, e.OutboxEventID, domain.ChannelWebPush, sub.ID)
		if err != nil {
			firstErr = errOr(firstErr, fmt.Errorf("web push: check delivery: %w", err))
			continue
		}
		if already {
			continue
		}

		status, err := w.send(ctx, sub, payload)
		if err != nil {
			firstErr = errOr(firstErr, fmt.Errorf("web push: send to %s: %w", sub.ID, err))
			continue
		}
		switch {
		case status >= 200 && status < 300:
			// Delivered — record it AFTER success (at-least-once: a crash here
			// re-sends next tick, never drops the notification).
			if err := w.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelWebPush, sub.ID); err != nil {
				firstErr = errOr(firstErr, fmt.Errorf("web push: record delivery: %w", err))
			}
		case status == 404 || status == 410:
			// The subscription is gone (browser unsubscribed / expired). Drop
			// the dead endpoint; this is NOT a retryable failure, so it does
			// not block the event from being marked processed.
			w.log.Info("web push: subscription gone, deleting",
				slog.String("subscription_id", sub.ID.String()), slog.Int("status", status))
			if err := w.subs.DeleteByID(ctx, sub.ID); err != nil {
				w.log.Error("web push: delete gone subscription failed",
					slog.String("subscription_id", sub.ID.String()), slog.String("error", err.Error()))
			}
		default:
			// A transient push-service error (429/5xx/etc.) — leave the event
			// unpublished so the next tick retries this subscription.
			firstErr = errOr(firstErr, fmt.Errorf("web push: send to %s got status %d", sub.ID, status))
		}
	}
	return firstErr
}

func buildPayload(e Event) pushPayload {
	name := e.GuestName
	if name == "" {
		name = "Гость"
	}
	local := e.StartsAt.Local().Format("02.01 15:04")
	return pushPayload{
		Title:        "Новая бронь",
		Body:         fmt.Sprintf("%s · %d чел · %s", name, e.Guests, local),
		Event:        string(e.Type),
		BookingID:    e.BookingID,
		RestaurantID: e.RestaurantID,
		Guests:       e.Guests,
		StartsAt:     e.StartsAt,
	}
}

func errOr(existing, err error) error {
	if existing != nil {
		return existing
	}
	return err
}
