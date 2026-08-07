package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// newTelegram builds a TelegramNotifier over the in-memory fakes, mirroring
// newWebPush.
func newTelegram(set *fakeSettings, del *fakeDeliveries, send TelegramSender, enabled bool) *TelegramNotifier {
	return NewTelegramNotifier(set, del, send, enabled, testLog())
}

// A new booking sends ONE telegram message to the restaurant's connected chat,
// with the booking details in the body.
func TestTelegram_SendsToRestaurantChat(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-1001234567890"
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}

	got := sender.sends()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1", len(got))
	}
	if got[0].chatID != "-1001234567890" {
		t.Fatalf("sent to chat %q, want -1001234567890", got[0].chatID)
	}
	// The message is the non-sensitive booking alert in Russian.
	if !strings.Contains(got[0].text, "Новая бронь") {
		t.Fatalf("message missing title: %q", got[0].text)
	}
	if !strings.Contains(got[0].text, "Damir") { // guest name from createdEvent
		t.Fatalf("message missing guest name: %q", got[0].text)
	}
	if !strings.Contains(got[0].text, "4") { // party size from createdEvent
		t.Fatalf("message missing party size: %q", got[0].text)
	}
}

// Interested reacts to both a new booking and a cancellation, and to nothing
// else — the cancellation is filtered further inside Notify.
func TestTelegram_InterestedInCreatedAndCancelled(t *testing.T) {
	tg := newTelegram(newFakeSettings(), newFakeDeliveries(), newRecordingTelegramSender().send, true)
	if !tg.Interested(domain.EventBookingCreated) {
		t.Fatal("must be interested in booking.created")
	}
	if !tg.Interested(domain.EventBookingCancelled) {
		t.Fatal("must be interested in booking.cancelled")
	}
	if tg.Interested(domain.EventBookingConfirmed) {
		t.Fatal("must NOT be interested in booking.confirmed")
	}
}

// A GUEST-cancelled booking is news the venue must act on (free the table): it
// IS sent, with the cancellation wording and the same detail block.
func TestTelegram_GuestCancelSendsToRestaurant(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-1001234567890"
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, err := toEvent(cancelledEvent(rest, domain.CancelledByGuest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}

	got := sender.sends()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1", len(got))
	}
	if got[0].chatID != "-1001234567890" {
		t.Fatalf("sent to chat %q, want -1001234567890", got[0].chatID)
	}
	if !strings.Contains(got[0].text, "отменена гостем") {
		t.Fatalf("guest-cancel must name the guest:\n%s", got[0].text)
	}
	if strings.Contains(got[0].text, "Новая бронь") {
		t.Fatalf("cancellation must not read as a new booking:\n%s", got[0].text)
	}
	// Reuses the shared detail block: name + party size + phone.
	if !strings.Contains(got[0].text, "Damir") || !strings.Contains(got[0].text, "Гостей: 4") {
		t.Fatalf("cancellation lost the booking details:\n%s", got[0].text)
	}
}

// A RESTAURANT-cancelled booking is NOT echoed back at the venue that just
// performed it: skipped before any send.
func TestTelegram_RestaurantCancelNoSend(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-1001234567890"
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(cancelledEvent(rest, domain.CancelledByRestaurant))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if n := len(sender.sends()); n != 0 {
		t.Fatalf("sent %d messages for a restaurant-side cancel, want 0", n)
	}
}

// An unknown (empty) or system CancelledBy defaults to sending — over-notifying
// is safer than dropping a genuine guest cancel when the field is missing — but
// the wording stays NEUTRAL: it must not mislabel the cancel as the guest's.
func TestTelegram_UnknownOrSystemCancelSendsNeutralTitle(t *testing.T) {
	for _, by := range []domain.CancelledBy{"", domain.CancelledBySystem} {
		by := by
		t.Run(string(by), func(t *testing.T) {
			rest := uuid.New()
			set := newFakeSettings()
			set.tgChat[rest] = "-1001234567890"
			sender := newRecordingTelegramSender()
			tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

			ev, _ := toEvent(cancelledEvent(rest, by))
			if err := tg.Notify(context.Background(), ev); err != nil {
				t.Fatalf("notify: %v", err)
			}
			got := sender.sends()
			if len(got) != 1 {
				t.Fatalf("sent %d messages, want 1", len(got))
			}
			if !strings.Contains(got[0].text, "❌ Бронь отменена") {
				t.Fatalf("neutral cancellation title missing:\n%s", got[0].text)
			}
			if strings.Contains(got[0].text, "гостем") {
				t.Fatalf("a non-guest cancel must NOT be labelled as the guest's:\n%s", got[0].text)
			}
		})
	}
}

// A restaurant with no connected chat id → nothing to send to → clean no-op.
func TestTelegram_NoChatIDNoOp(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings() // no tgChat entry
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(sender.sends()) != 0 {
		t.Fatal("a message was sent for a restaurant with no chat id")
	}
}

// The per-restaurant toggle: telegram disabled for a venue → no send even with
// a chat id connected.
func TestTelegram_RestaurantToggleDisabled(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	set.tgDisabled[rest] = true
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(sender.sends()) != 0 {
		t.Fatal("a message was sent for a restaurant with telegram disabled")
	}
}

// Missing bot token (enabled=false) → the notifier is disabled → clean no-op,
// no send, no error, no crash.
func TestTelegram_MissingTokenNoOp(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, false)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify with no token should no-op, got %v", err)
	}
	if len(sender.sends()) != 0 {
		t.Fatal("a message was sent despite a missing bot token")
	}
}

// A nil sender (also "not configured") is treated as disabled, no panic.
func TestTelegram_NilSenderNoOp(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	tg := newTelegram(set, newFakeDeliveries(), nil, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("nil sender should no-op, got %v", err)
	}
}

// Cross-tenant: restaurant B's chat never receives restaurant A's booking.
func TestTelegram_NoCrossTenant(t *testing.T) {
	restA, restB := uuid.New(), uuid.New()
	set := newFakeSettings()
	set.tgChat[restA] = "-100AAA"
	set.tgChat[restB] = "-100BBB"
	sender := newRecordingTelegramSender()
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(createdEvent(restA))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	got := sender.sends()
	if len(got) != 1 || got[0].chatID != "-100AAA" {
		t.Fatalf("sent = %v, want only restaurant A's chat -100AAA", got)
	}
}

// Dedup: a redelivery of the same outbox event never double-sends to the chat.
func TestTelegram_DedupRedeliveryNoDoubleSend(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	sender := newRecordingTelegramSender()
	del := newFakeDeliveries()
	tg := newTelegram(set, del, sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify 1: %v", err)
	}
	// Same event again (a sibling channel failed → the outbox row stayed
	// unpublished → this event is re-run next tick).
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify 2: %v", err)
	}
	if n := len(sender.sends()); n != 1 {
		t.Fatalf("sent %d messages on redelivery, want exactly 1 (dedup)", n)
	}
}

// A transient Bot API status (429/5xx) is retryable: Notify returns an error so
// the dispatcher leaves the event unpublished; the delivery is NOT recorded.
func TestTelegram_TransientStatusIsRetryable(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	sender := newRecordingTelegramSender()
	sender.status["-100999"] = 429
	del := newFakeDeliveries()
	tg := newTelegram(set, del, sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err == nil {
		t.Fatal("a 429 must surface as a retryable error, got nil")
	}
	// Not recorded → a later retry will send again.
	if already, _ := del.AlreadyDelivered(context.Background(), ev.OutboxEventID, domain.ChannelTelegram, rest); already {
		t.Fatal("a failed send must not be recorded as delivered")
	}
}

// A 403 (bot blocked / removed from the chat) is NOT retryable: the event is
// consumed (nil error) so it does not stall the outbox, but it is not recorded
// as delivered either.
func TestTelegram_BlockedChatNotRetryable(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	sender := newRecordingTelegramSender()
	sender.status["-100999"] = 403
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("a blocked chat must not be a retryable error, got %v", err)
	}
}

// A transport error (timeout/DNS) surfaces as a retryable error.
func TestTelegram_TransportErrorIsRetryable(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	sender := newRecordingTelegramSender()
	sender.errFor["-100999"] = errors.New("dial timeout")
	tg := newTelegram(set, newFakeDeliveries(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := tg.Notify(context.Background(), ev); err == nil {
		t.Fatal("a transport error must surface as a retryable error, got nil")
	}
}
