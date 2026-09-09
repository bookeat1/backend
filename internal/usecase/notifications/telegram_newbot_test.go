package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// markReady puts a venue on the new bot the same way the new bot's webhook
// does: by writing telegram_new_bot_ready_at.
func markReady(t *testing.T, set *fakeSettings, rest uuid.UUID) {
	t.Helper()
	if err := set.MarkTelegramNewBotReady(context.Background(), rest); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
}

// A venue that has NOT pressed Start keeps receiving from the OLD bot even
// though the new one is fully configured. This is the default state of every
// venue on the day of the deploy, so it is the case that must not break.
func TestTelegramNewBot_UnmigratedVenueStaysOnOldBot(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100111"
	oldBot := newRecordingTelegramSender()
	newBot := newRecordingTelegramSender()

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true).
		WithNewBot(newBot.send, nil, nil)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := len(newBot.sends()); got != 0 {
		t.Fatalf("new bot sent %d messages to an unmigrated venue, want 0", got)
	}
	if got := len(oldBot.sends()); got != 1 {
		t.Fatalf("old bot sent %d messages, want 1", got)
	}
}

// A migrated venue receives from the NEW bot, and the old bot stays silent:
// exactly one message reaches the chat, never two.
func TestTelegramNewBot_ReadyVenueGetsExactlyOneMessageFromNewBot(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100222"
	markReady(t, set, rest)
	oldBot := newRecordingTelegramSender()
	newBot := newRecordingTelegramSender()

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true).
		WithNewBot(newBot.send, nil, nil)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := newBot.sends(); len(got) != 1 || got[0].chatID != "-100222" {
		t.Fatalf("new bot sends = %+v, want exactly one to -100222", got)
	}
	if got := len(oldBot.sends()); got != 0 {
		t.Fatalf("old bot also sent %d messages — the venue would see the alert twice", got)
	}
}

// THE case the whole staged migration exists for: the new bot is refused with
// 403 ("bot can't initiate conversation"). The event must NOT be lost — it goes
// out through the old bot — and the venue must be demoted so the next event
// does not waste another attempt.
func TestTelegramNewBot_403FallsBackToOldBotAndDemotes(t *testing.T) {
	for _, status := range []int{400, 403} {
		rest := uuid.New()
		set := newFakeSettings()
		set.tgChat[rest] = "-100333"
		markReady(t, set, rest)
		oldBot := newRecordingTelegramSender()
		newBot := newRecordingTelegramSender()
		newBot.status["-100333"] = status

		del := newFakeDeliveries()
		tg := newTelegram(set, del, oldBot.send, true).
			WithNewBot(newBot.send, nil, nil)

		ev, err := toEvent(createdEvent(rest))
		if err != nil {
			t.Fatalf("toEvent: %v", err)
		}
		if err := tg.Notify(context.Background(), ev); err != nil {
			t.Fatalf("status %d: notify returned %v, want nil (event handled)", status, err)
		}
		if got := len(newBot.sends()); got != 1 {
			t.Fatalf("status %d: new bot attempts = %d, want 1", status, got)
		}
		if got := oldBot.sends(); len(got) != 1 || got[0].chatID != "-100333" {
			t.Fatalf("status %d: the alert was NOT delivered by the old bot: %+v", status, got)
		}
		// Demoted: ready_at cleared, failed_at written.
		cfg, err := set.TelegramSettings(context.Background(), rest)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if cfg.NewBotReady() {
			t.Fatalf("status %d: venue is still marked ready after a refusal", status)
		}
		if cfg.NewBotFailedAt == nil {
			t.Fatalf("status %d: the refusal was not recorded", status)
		}
		// And the delivery is recorded exactly once, so a redelivery of the same
		// outbox event does not re-send.
		already, err := del.AlreadyDelivered(context.Background(), ev.OutboxEventID, domain.ChannelTelegram, rest)
		if err != nil {
			t.Fatalf("already delivered: %v", err)
		}
		if !already {
			t.Fatalf("status %d: delivery was not recorded after the fallback send", status)
		}
	}
}

// After a demotion the NEXT event goes straight to the old bot — the new bot is
// not tried again until staff press Start.
func TestTelegramNewBot_DemotionSticksForTheNextEvent(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100444"
	markReady(t, set, rest)
	oldBot := newRecordingTelegramSender()
	newBot := newRecordingTelegramSender()
	newBot.status["-100444"] = 403

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true).
		WithNewBot(newBot.send, nil, nil)

	for i := 0; i < 2; i++ {
		ev, err := toEvent(createdEvent(rest))
		if err != nil {
			t.Fatalf("toEvent: %v", err)
		}
		if err := tg.Notify(context.Background(), ev); err != nil {
			t.Fatalf("notify #%d: %v", i, err)
		}
	}
	if got := len(newBot.sends()); got != 1 {
		t.Fatalf("new bot was tried %d times, want 1 (demotion must stick)", got)
	}
	if got := len(oldBot.sends()); got != 2 {
		t.Fatalf("old bot delivered %d of 2 events", got)
	}
}

// A 429/5xx from the new bot is Telegram being busy, not a verdict about this
// chat: the event stays unpublished for a retry and the venue is NOT demoted.
// Falling back here would burn the migration flag on a transient blip.
func TestTelegramNewBot_TransientStatusRetriesAndKeepsReady(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100555"
	markReady(t, set, rest)
	oldBot := newRecordingTelegramSender()
	newBot := newRecordingTelegramSender()
	newBot.status["-100555"] = 500

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true).
		WithNewBot(newBot.send, nil, nil)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err == nil {
		t.Fatal("a 500 from the new bot must be retryable, got nil")
	}
	if got := len(oldBot.sends()); got != 0 {
		t.Fatalf("old bot sent %d messages on a transient failure, want 0", got)
	}
	cfg, err := set.TelegramSettings(context.Background(), rest)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !cfg.NewBotReady() {
		t.Fatal("a transient failure must not demote the venue")
	}
}

// A transport error (timeout/DNS) from the new bot is retryable for the same
// reason and must not silently drop the alert.
func TestTelegramNewBot_TransportErrorIsRetryable(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100666"
	markReady(t, set, rest)
	oldBot := newRecordingTelegramSender()
	newBot := newRecordingTelegramSender()
	newBot.errFor["-100666"] = errors.New("dial tcp: i/o timeout")

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true).
		WithNewBot(newBot.send, nil, nil)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err == nil {
		t.Fatal("a transport error must be retryable, got nil")
	}
	if got := len(oldBot.sends()); got != 0 {
		t.Fatalf("old bot sent %d messages on a transport error, want 0", got)
	}
}

// Buttons under the new bot's alert carry the SAME callback format, so a press
// is understood by whichever webhook receives it.
func TestTelegramNewBot_SendsWithButtonsOnCreated(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100777"
	markReady(t, set, rest)
	newBot := newRecordingTelegramSender()

	var gotActions [][2]string
	sendWith := func(_ context.Context, chatID, text string, actions [][2]string) (int, error) {
		gotActions = actions
		return 200, nil
	}
	tg := newTelegram(set, newFakeDeliveries(), newRecordingTelegramSender().send, true).
		WithNewBot(newBot.send, sendWith, func(id uuid.UUID) [][2]string {
			return [][2]string{{"Подтвердить", "bk:confirm:" + id.String()}}
		})

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(gotActions) != 1 {
		t.Fatalf("new bot sent %d buttons, want 1", len(gotActions))
	}
}

// Deployment with ONLY the new bot: a refused venue has nowhere to fall back
// to. The event is still drained (never blocks the outbox) — the same contract
// a 403 had before the migration — and nothing panics on the nil old sender.
func TestTelegramNewBot_NoOldBotConfiguredDoesNotPanic(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100888"
	markReady(t, set, rest)
	newBot := newRecordingTelegramSender()
	newBot.status["-100888"] = 403

	tg := NewTelegramNotifier(set, newFakeDeliveries(), nil, false, testLog()).
		WithNewBot(newBot.send, nil, nil)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

// Without the new bot's token nothing about the old behaviour changes: same
// single send by the old bot, no new failure mode. (Acceptance criterion 39.)
func TestTelegramNewBot_AbsentTokenChangesNothing(t *testing.T) {
	rest := uuid.New()
	set := newFakeSettings()
	set.tgChat[rest] = "-100999"
	// A stale ready flag from a previous deploy must not make an unconfigured
	// new bot be used.
	ready := time.Now()
	set.tgNewBotReady[rest] = &ready
	oldBot := newRecordingTelegramSender()

	tg := newTelegram(set, newFakeDeliveries(), oldBot.send, true)

	ev, err := toEvent(createdEvent(rest))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := len(oldBot.sends()); got != 1 {
		t.Fatalf("old bot sent %d messages, want 1", got)
	}
}
