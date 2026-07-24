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
