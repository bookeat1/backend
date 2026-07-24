package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"backend-core/internal/domain"
)

// TelegramSender delivers one plain-text message to a Telegram chat and reports
// the Bot API HTTP status. It is the seam that keeps the Telegram Bot API (and
// the network) out of the notifier's logic, so the fan-out / dedupe /
// tenant-scoping behaviour is unit-testable without a real bot. statusCode is
// the Bot API response code (2xx = accepted; 400/403 = a bad/blocked chat, not
// retryable; 429/5xx = transient, retry).
type TelegramSender func(ctx context.Context, chatID string, text string) (statusCode int, err error)

// TelegramNotifier is the Telegram channel: on a new booking it sends ONE
// message to that booking's restaurant chat, and no other venue's chat — the
// target is resolved from the booking's own restaurant_id, so there is no
// cross-tenant alert.
//
// When the bot token is absent the notifier is DISABLED: Notify logs once and
// no-ops, returning nil, so the dispatcher marks the event processed and the
// worker never crashes for lack of a token (the owner provisions it later),
// exactly like the web-push channel without VAPID keys.
type TelegramNotifier struct {
	settings   domain.RestaurantNotificationSettingsRepository
	deliveries domain.NotificationDeliveryRepository
	send       TelegramSender
	enabled    bool // bot token configured
	log        *slog.Logger
}

// NewTelegramNotifier builds the Telegram channel. Pass enabled=false (or a nil
// sender) to run it as a clean no-op when TELEGRAM_NOTIFY_BOT_TOKEN is unset.
func NewTelegramNotifier(
	settings domain.RestaurantNotificationSettingsRepository,
	deliveries domain.NotificationDeliveryRepository,
	send TelegramSender,
	enabled bool,
	log *slog.Logger,
) *TelegramNotifier {
	return &TelegramNotifier{
		settings: settings, deliveries: deliveries,
		send: send, enabled: enabled && send != nil, log: log,
	}
}

var _ Notifier = (*TelegramNotifier)(nil)

func (t *TelegramNotifier) Channel() domain.NotificationChannel { return domain.ChannelTelegram }

// Interested: increment 1 alerts staff only on a NEW booking.
func (t *TelegramNotifier) Interested(et domain.BookingEventType) bool {
	return et == domain.EventBookingCreated
}

func (t *TelegramNotifier) Notify(ctx context.Context, e Event) error {
	if !t.enabled {
		t.log.Debug("telegram disabled (no bot token), skipping",
			slog.String("booking_id", e.BookingID.String()))
		return nil
	}

	cfg, err := t.settings.TelegramSettings(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("telegram: read settings: %w", err)
	}
	if !cfg.Enabled {
		t.log.Debug("telegram disabled for restaurant, skipping",
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}
	if cfg.ChatID == "" {
		// No chat connected yet — nothing to send to. Not an error: the event is
		// still processed (drained) so it never blocks the outbox.
		t.log.Debug("telegram has no chat id for restaurant, skipping",
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	// The dedupe target for telegram is the restaurant (one chat per venue).
	already, err := t.deliveries.AlreadyDelivered(ctx, e.OutboxEventID, domain.ChannelTelegram, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("telegram: check delivery: %w", err)
	}
	if already {
		return nil
	}

	status, err := t.send(ctx, cfg.ChatID, buildTelegramText(e))
	if err != nil {
		// A transport error (timeout/DNS/etc.) is retryable — leave the event
		// unpublished so the next tick retries.
		return fmt.Errorf("telegram: send to restaurant %s: %w", e.RestaurantID, err)
	}
	switch {
	case status >= 200 && status < 300:
		// Delivered — record AFTER success (at-least-once: a crash here re-sends
		// next tick, never drops the notification).
		if err := t.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelTelegram, e.RestaurantID); err != nil {
			return fmt.Errorf("telegram: record delivery: %w", err)
		}
		return nil
	case status == 400 || status == 403:
		// Bad or blocked chat (wrong chat id, bot removed from the group, bot
		// blocked by the user). NOT retryable — retrying can never succeed until
		// staff fix the chat id, and blocking the outbox on it would stall every
		// other event. Log and let the event be marked processed.
		t.log.Warn("telegram: chat rejected the message, giving up on this event",
			slog.String("restaurant_id", e.RestaurantID.String()), slog.Int("status", status))
		return nil
	default:
		// 429 / 5xx / anything else — transient. Retry on the next tick.
		return fmt.Errorf("telegram: send to restaurant %s got status %d", e.RestaurantID, status)
	}
}

// buildTelegramText renders the non-sensitive booking alert in Russian. No OTP,
// no payment data — only what staff already see in the venue cabinet: that a
// booking came in, for when, for how many, under what name.
func buildTelegramText(e Event) string {
	name := e.GuestName
	if name == "" {
		name = "Гость"
	}
	local := e.StartsAt.Local().Format("02.01 15:04")
	return fmt.Sprintf("Новая бронь\nВремя: %s\nГостей: %d\nИмя: %s", local, e.Guests, name)
}
