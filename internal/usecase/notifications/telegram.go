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

	// --- staged migration to @book_eat_restaurants_bot (spec §7) ---
	//
	// The NEW bot lives BESIDE the old one, never instead of it. A venue is
	// moved over one at a time, and only after the new bot has proved it can
	// write to that chat (telegram_new_bot_ready_at). Everything below is nil /
	// false until RESTAURANTS_BOT_TOKEN is configured, in which case this
	// notifier behaves exactly as it did before the migration existed.
	newSend     TelegramSender
	newSendWith TelegramActionSender
	newActions  TelegramActions
	newEnabled  bool
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

// WithNewBot arms the staged migration to the second (restaurants) bot. send is
// required; sendWith/actions are optional and follow the same rule as
// WithActions — buttons only when the venue can actually answer them, which for
// the new bot means its OWN webhook is configured.
//
// Arming this changes nothing for a venue whose telegram_new_bot_ready_at is
// NULL: it keeps receiving alerts from the old bot until it presses Start.
func (t *TelegramNotifier) WithNewBot(send TelegramSender, sendWith TelegramActionSender, actions TelegramActions) *TelegramNotifier {
	if send == nil {
		return t
	}
	t.newSend, t.newEnabled = send, true
	if sendWith != nil && actions != nil {
		t.newSendWith, t.newActions = sendWith, actions
	}
	// The new bot alone is enough to run the channel: a deployment that only
	// ever had the new token must still notify.
	t.enabled = true
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
	// Buttons only on a NEW booking: that is the only event the venue is being
	// asked to answer. A cancellation alert with a "Confirm" button under it
	// would be nonsense.
	withButtons := e.Type == domain.EventBookingCreated

	// --- attempt 1: the NEW bot, but only for a venue already migrated ---
	//
	// A venue is migrated when its own staff proved it, never when we assumed
	// it: telegram_new_bot_ready_at is written by the new bot's webhook.
	if t.newEnabled && cfg.NewBotReady() {
		status, err := t.deliver(ctx, t.newSend, t.newSendWith, t.newActions, cfg.ChatID, text, e, withButtons)
		if err != nil {
			// Transport error — retryable, and NOT a reason to demote the venue:
			// a DNS blip says nothing about whether the bot may write here.
			return fmt.Errorf("telegram: send to restaurant %s via new bot: %w", e.RestaurantID, err)
		}
		switch {
		case status >= 200 && status < 300:
			return t.recordDelivered(ctx, e)
		case status == 400 || status == 403:
			// The new bot is refused by this chat (never started, kicked out of
			// the group, blocked). Demote the venue back to the old bot and FALL
			// THROUGH — the whole point of the staged migration is that this
			// event still gets delivered, by the bot that can.
			t.log.Warn("telegram.new_bot_rejected",
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("booking_id", e.BookingID.String()),
				slog.Int("status", status))
			if err := t.settings.MarkTelegramNewBotFailed(ctx, e.RestaurantID); err != nil {
				// Not fatal and deliberately not returned: failing here would
				// retry the whole event and re-send through the old bot below on
				// the next tick anyway. Worst case the demotion is retried on the
				// next event.
				t.log.Error("telegram: could not record the new bot's refusal",
					slog.String("restaurant_id", e.RestaurantID.String()),
					slog.String("error", err.Error()))
			}
		default:
			// 429 / 5xx — transient at Telegram, not a verdict about this chat.
			// Retry the same event later rather than burning the fallback on it.
			return fmt.Errorf("telegram: send to restaurant %s via new bot got status %d", e.RestaurantID, status)
		}
	}

	// --- attempt 2: the OLD bot — the safety net for every unmigrated venue ---
	if !t.enabledOldBot() {
		// Only the new bot is configured and it just refused (or the venue is not
		// migrated at all). There is nothing to fall back to; the event is drained
		// rather than blocking the outbox forever, exactly as a 403 was drained
		// before the migration. Loud, because this is a real undelivered alert.
		t.log.Warn("telegram: no fallback bot configured, alert not delivered",
			slog.String("restaurant_id", e.RestaurantID.String()),
			slog.String("booking_id", e.BookingID.String()))
		return nil
	}
	status, err := t.deliver(ctx, t.send, t.sendWith, t.actions, cfg.ChatID, text, e, withButtons)
	if err != nil {
		// A transport error (timeout/DNS/etc.) is retryable — leave the event
		// unpublished so the next tick retries.
		return fmt.Errorf("telegram: send to restaurant %s: %w", e.RestaurantID, err)
	}
	switch {
	case status >= 200 && status < 300:
		return t.recordDelivered(ctx, e)
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

// enabledOldBot reports whether the original notifications bot can still send.
// It is a method, not a field, so the "is there a fallback" question has exactly
// one answer everywhere.
func (t *TelegramNotifier) enabledOldBot() bool { return t.send != nil }

// deliver performs one send through the given pair of senders, choosing the
// keyboard variant. Extracted so the new-bot and old-bot attempts cannot drift
// apart in how they build a message.
func (t *TelegramNotifier) deliver(
	ctx context.Context,
	send TelegramSender, sendWith TelegramActionSender, actions TelegramActions,
	chatID, text string, e Event, withButtons bool,
) (int, error) {
	if withButtons && sendWith != nil && actions != nil {
		return sendWith(ctx, chatID, text, actions(e.BookingID))
	}
	return send(ctx, chatID, text)
}

// recordDelivered writes the dedupe marker AFTER a successful send
// (at-least-once: a crash here re-sends next tick, never drops the
// notification). One marker per event+venue, whichever bot delivered it — so a
// venue can never receive the same alert from both bots.
func (t *TelegramNotifier) recordDelivered(ctx context.Context, e Event) error {
	if err := t.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelTelegram, e.RestaurantID); err != nil {
		return fmt.Errorf("telegram: record delivery: %w", err)
	}
	return nil
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
