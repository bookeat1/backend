package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TelegramSender delivers one plain-text message to a Telegram chat and reports
// the Bot API HTTP status. It is the seam that keeps the Telegram Bot API (and
// the network) out of the notifier's logic, so the fan-out / dedupe /
// tenant-scoping behaviour is unit-testable without a real bot. statusCode is
// the Bot API response code (2xx = accepted; 400/403 = a bad/blocked chat, not
// retryable; 429/5xx = transient, retry).
type TelegramSender func(ctx context.Context, chatID string, text string) (statusCode int, err error)

// TelegramActionSender is the same send with an inline keyboard underneath.
// Each action is a {label, callback data} pair, opaque to this package. When it
// is nil the notifier falls back to the plain sender, so a deployment without
// the answer webhook still gets alerts — just without buttons to press.
type TelegramActionSender func(ctx context.Context, chatID, text string, actions [][2]string) (statusCode int, err error)

// TelegramActions builds the buttons for one booking. It lives outside this
// package (the transport layer owns the callback format) and is optional for
// the same reason TelegramActionSender is.
type TelegramActions func(bookingID uuid.UUID) [][2]string

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
	sendWith   TelegramActionSender
	actions    TelegramActions
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

// WithActions turns the alert's buttons on. Both arguments must be non-nil:
// buttons the venue cannot answer (no webhook) would be worse than no buttons,
// because staff would press them and nothing would happen.
func (t *TelegramNotifier) WithActions(send TelegramActionSender, actions TelegramActions) *TelegramNotifier {
	if send != nil && actions != nil {
		t.sendWith, t.actions = send, actions
	}
	return t
}

var _ Notifier = (*TelegramNotifier)(nil)

func (t *TelegramNotifier) Channel() domain.NotificationChannel { return domain.ChannelTelegram }

// Interested: the staff channel reacts to a NEW booking (the venue is asked to
// answer it) and to a CANCELLATION (the venue needs to free the table). The
// cancellation is filtered further in Notify — see the CancelledBy skip.
func (t *TelegramNotifier) Interested(et domain.BookingEventType) bool {
	return et == domain.EventBookingCreated || et == domain.EventBookingCancelled
}

func (t *TelegramNotifier) Notify(ctx context.Context, e Event) error {
	if !t.enabled {
		t.log.Info("telegram skipped: no bot token configured",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	// PRODUCT RULE: a cancellation is pushed to the venue ONLY when the GUEST
	// cancelled — that is news the venue must act on (free the table). A
	// restaurant-side cancellation (staff pressed the Telegram button, hit
	// Reject in the venue cabinet, etc.) is NOT echoed back at them: they just
	// performed it and already know. This mirrors the guest channel, which
	// suppresses the echo of a cancellation the guest performed themselves.
	//
	// ASSUMPTION: an empty/unknown CancelledBy (and a system cancellation) still
	// sends — over-notifying the venue is safer than silently dropping a genuine
	// guest cancel when the field is missing.
	if e.Type == domain.EventBookingCancelled && e.CancelledBy == domain.CancelledByRestaurant {
		t.log.Info("telegram skipped: restaurant cancelled its own booking, no echo",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	cfg, err := t.settings.TelegramSettings(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("telegram: read settings: %w", err)
	}
	if !cfg.Enabled {
		t.log.Info("telegram skipped: channel switched off for this venue",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}
	if cfg.ChatID == "" {
		// No chat connected yet — nothing to send to. Not an error: the event is
		// still processed (drained) so it never blocks the outbox.
		t.log.Info("telegram skipped: venue has not connected a chat",
			slog.String("booking_id", e.BookingID.String()),
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

	text := buildTelegramText(e)
	var status int
	if t.sendWith != nil && t.actions != nil && e.Type == domain.EventBookingCreated {
		// Buttons only on a NEW booking: that is the only event the venue is
		// being asked to answer. A cancellation alert with a "Confirm" button
		// under it would be nonsense.
		status, err = t.sendWith(ctx, cfg.ChatID, text, t.actions(e.BookingID))
	} else {
		status, err = t.send(ctx, cfg.ChatID, text)
	}
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
	title := "Новая бронь"
	if e.Type == domain.EventBookingCancelled {
		// A restaurant-side cancel is filtered out before send, so here the actor
		// is guest, system, or unknown. Name the guest only when we actually know
		// it was the guest; otherwise stay neutral rather than mislabel a system
		// (or unknown-actor) cancellation as the guest's doing.
		if e.CancelledBy == domain.CancelledByGuest {
			title = "❌ Бронь отменена гостем"
		} else {
			title = "❌ Бронь отменена"
		}
	}
	return title + "\n" + telegramBookingDetails(e)
}

// telegramBookingDetails renders the shared, non-sensitive detail block reused
// by every staff alert: when, how many, under what name, and — for staff only —
// the phone. Kept separate so the "created" and "cancelled" messages cannot
// drift apart in formatting.
func telegramBookingDetails(e Event) string {
	name := e.GuestName
	if name == "" {
		name = "Гость"
	}
	local := e.StartsAt.Local().Format("02.01 15:04")
	text := fmt.Sprintf("Время: %s\nГостей: %d\nИмя: %s", local, e.Guests, name)
	// The phone is the difference between an alert staff can act on and one
	// that sends them to the panel: it is what they use to confirm a large
	// party or to find a guest who has not arrived. Omitted, not blanked, when
	// a booking has no number at all — an empty "Телефон:" line reads like a
	// bug in the message.
	if e.GuestPhone != "" {
		text += "\nТелефон: " + e.GuestPhone
	}
	return text
}
