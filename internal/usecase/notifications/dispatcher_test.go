package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newWebPush(subs *fakeSubs, del *fakeDeliveries, set *fakeSettings, send PushSender, enabled bool) *WebPushNotifier {
	return NewWebPushNotifier(subs, del, set, send, enabled, testLog())
}

// A new-booking event is claimed and dispatched exactly once, then marked
// published so a second tick does not re-dispatch it.
func TestDispatcher_DispatchesOnceThenMarksPublished(t *testing.T) {
	restaurant := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), UserID: uuid.New(), RestaurantID: restaurant, Endpoint: "e", P256dh: "p", Auth: "a"}
	ev := createdEvent(restaurant)

	outbox := newFakeOutbox(ev)
	sender := newRecordingSender()
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), newFakeSettings(), sender.send, true)
	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{}, testLog(), wp)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if res.Dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (%+v)", res.Dispatched, res)
	}
	if !outbox.isPublished(ev.ID) {
		t.Fatal("event not marked published after a successful dispatch")
	}
	if got := sender.sentIDs(); len(got) != 1 || got[0] != sub.ID {
		t.Fatalf("sent = %v, want one push to %s", got, sub.ID)
	}

	// Second tick: the event is published, nothing to claim, no extra push.
	res2, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res2 != (TickResult{}) {
		t.Fatalf("second tick did work: %+v", res2)
	}
	if len(sender.sentIDs()) != 1 {
		t.Fatalf("push sent %d times across two ticks, want exactly 1", len(sender.sentIDs()))
	}
}

// A send failure on one subscription leaves the event UNPUBLISHED (retried),
// and the redelivery does not re-notify the subscription that already
// succeeded — only the failed one is retried.
func TestDispatcher_FailureRetriesWithoutDoubleNotify(t *testing.T) {
	restaurant := uuid.New()
	subOK := domain.PushSubscription{ID: uuid.New(), RestaurantID: restaurant, Endpoint: "ok", P256dh: "p", Auth: "a"}
	subBad := domain.PushSubscription{ID: uuid.New(), RestaurantID: restaurant, Endpoint: "bad", P256dh: "p", Auth: "a"}
	ev := createdEvent(restaurant)

	outbox := newFakeOutbox(ev)
	sender := newRecordingSender()
	sender.errFor[subBad.ID] = errors.New("push service timeout")
	del := newFakeDeliveries()
	wp := newWebPush(newFakeSubs(subOK, subBad), del, newFakeSettings(), sender.send, true)
	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{}, testLog(), wp)
	// The retry happens after the backoff, not on the immediately next tick, so
	// the clock is stepped by hand.
	now := time.Now()
	d.now = func() time.Time { return now }

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if res.Retry != 1 || res.Dispatched != 0 {
		t.Fatalf("tick 1 result = %+v, want one retry", res)
	}
	if outbox.isPublished(ev.ID) {
		t.Fatal("event marked published despite a failed send — it would never retry")
	}

	// Repair the failing subscription and re-tick once the backoff elapsed
	// (redelivery of the same event).
	sender.errFor = map[uuid.UUID]error{}
	now = now.Add(defaultRetryBaseDelay)
	res2, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res2.Dispatched != 1 {
		t.Fatalf("tick 2 result = %+v, want one dispatched", res2)
	}
	if !outbox.isPublished(ev.ID) {
		t.Fatal("event still not published after the retry succeeded")
	}

	// subOK must have been pushed exactly once (dedupe), subBad twice
	// (first failed, second succeeded).
	counts := map[uuid.UUID]int{}
	for _, id := range sender.sentIDs() {
		counts[id]++
	}
	if counts[subOK.ID] != 1 {
		t.Fatalf("subOK pushed %d times, want 1 (no double-notify on redelivery)", counts[subOK.ID])
	}
	if counts[subBad.ID] != 2 {
		t.Fatalf("subBad pushed %d times, want 2 (retry of only the failed one)", counts[subBad.ID])
	}
}

// An event no notifier cares about is drained (marked published) so it never
// blocks the outbox head.
func TestDispatcher_DrainsUninterestingEvents(t *testing.T) {
	restaurant := uuid.New()
	ev := createdEvent(restaurant)
	ev.EventType = domain.EventBookingCancelled // web push is not interested

	outbox := newFakeOutbox(ev)
	sender := newRecordingSender()
	wp := newWebPush(newFakeSubs(), newFakeDeliveries(), newFakeSettings(), sender.send, true)
	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{}, testLog(), wp)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Drained != 1 {
		t.Fatalf("result = %+v, want one drained", res)
	}
	if !outbox.isPublished(ev.ID) {
		t.Fatal("uninteresting event not drained — it would re-claim forever")
	}
	if len(sender.sentIDs()) != 0 {
		t.Fatal("a push was sent for an uninteresting event")
	}
}

// An undecodable payload is marked published (poison-pill guard) and never sent.
func TestDispatcher_PoisonPayloadDoesNotBlock(t *testing.T) {
	ev := createdEvent(uuid.New())
	ev.Payload = []byte(`{not json`)

	outbox := newFakeOutbox(ev)
	sender := newRecordingSender()
	wp := newWebPush(newFakeSubs(), newFakeDeliveries(), newFakeSettings(), sender.send, true)
	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{}, testLog(), wp)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Poison != 1 {
		t.Fatalf("result = %+v, want one poison", res)
	}
	if !outbox.isPublished(ev.ID) {
		t.Fatal("poison event not marked published — it would block the outbox forever")
	}
}

// THE REGRESSION TEST for the head-of-line blocking this file's fairness rules
// exist to prevent.
//
// Setup is the outage that #98 made reachable: WhatsApp is down for everyone,
// its events are the OLDEST in the outbox, and there are exactly as many of
// them as the batch holds. Before the fix the claim was
// `WHERE published_at IS NULL ORDER BY created_at LIMIT batch`, so every tick
// re-read the same failing WhatsApp events and the newer Telegram ones were
// never claimed at all — a Meta outage silently took down every other channel.
//
// After the fix the failed events are pushed past next_attempt_at and fresh
// events sort first, so the second tick delivers the Telegram events. The clock
// is deliberately NOT advanced between ticks: the newer events must go out
// DURING the outage, not after it.
func TestDispatcher_FailingChannelDoesNotStarveOthers(t *testing.T) {
	restaurant := uuid.New()
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// Two old booking.created events — only the broken channel wants them.
	old1, old2 := createdEvent(restaurant), createdEvent(restaurant)
	old1.CreatedAt, old2.CreatedAt = base.Add(-2*time.Hour), base.Add(-time.Hour)
	// Two newer booking.cancelled events — only the healthy channel wants them.
	new1 := cancelledEvent(restaurant, domain.CancelledByRestaurant)
	new2 := cancelledEvent(restaurant, domain.CancelledByRestaurant)
	new1.CreatedAt, new2.CreatedAt = base.Add(-10*time.Minute), base.Add(-5*time.Minute)

	outbox := newFakeOutbox(old1, old2, new1, new2)
	broken := newStubNotifier(domain.ChannelWhatsApp, errors.New("meta: 429 rate limited"),
		domain.EventBookingCreated)
	healthy := newStubNotifier(domain.ChannelTelegram, nil, domain.EventBookingCancelled)

	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{BatchSize: 2}, testLog(), broken, healthy)
	d.now = func() time.Time { return base } // frozen clock: the outage is still on

	res1, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if res1.Retry != 2 {
		t.Fatalf("tick 1 = %+v, want the two whatsapp events retried", res1)
	}

	res2, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := len(healthy.delivered()); got != 2 {
		t.Fatalf("healthy channel delivered %d of 2 newer events while the other channel is down "+
			"(tick 2 = %+v): a one-channel outage is starving every other channel", got, res2)
	}
	if !outbox.isPublished(new1.ID) || !outbox.isPublished(new2.ID) {
		t.Fatal("newer events not published even though their only channel succeeded")
	}
	// The broken channel's events are not lost: still unpublished, counted, and
	// scheduled for a later attempt rather than hammered every tick.
	for _, ev := range []domain.BookingOutboxEvent{old1, old2} {
		if outbox.isPublished(ev.ID) {
			t.Fatalf("event %s published despite the send failing — the alert would be lost", ev.ID)
		}
		if got := outbox.attemptsOf(ev.ID); got != 1 {
			t.Fatalf("event %s attempts = %d, want 1", ev.ID, got)
		}
		due, ok := outbox.dueAt(ev.ID)
		if !ok || !due.After(base) {
			t.Fatalf("event %s next attempt = %v (set=%v), want a moment after now", ev.ID, due, ok)
		}
	}
	if got := len(broken.delivered()); got != 2 {
		t.Fatalf("broken channel was called %d times across two ticks, want 2 (one per event): "+
			"a failing event must not be re-attempted before its backoff elapses", got)
	}
}

// The backoff grows and the event becomes claimable again once it elapses —
// a retry is delayed, never dropped.
func TestDispatcher_RetryBackoffGrowsAndBecomesDueAgain(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	ev := createdEvent(uuid.New())
	ev.CreatedAt = base.Add(-time.Hour)

	outbox := newFakeOutbox(ev)
	broken := newStubNotifier(domain.ChannelWhatsApp, errors.New("meta: 503"), domain.EventBookingCreated)
	cfg := DispatcherConfig{BatchSize: 10, RetryBaseDelay: time.Minute, RetryMaxDelay: 4 * time.Minute, MaxAttempts: 5}

	now := base
	d := NewDispatcher(outbox, noopTx{}, cfg, testLog(), broken)
	d.now = func() time.Time { return now }

	// base delay after the 1st failure, doubling after the 2nd, capped at the 3rd.
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		res, err := d.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if res.Retry != 1 {
			t.Fatalf("tick %d = %+v, want one retry", i+1, res)
		}
		due, _ := outbox.dueAt(ev.ID)
		if !due.Equal(now.Add(want)) {
			t.Fatalf("after failure %d next attempt = %v, want %v", i+1, due.Sub(now), want)
		}
		// A tick BEFORE the backoff elapses must not touch the event at all.
		now = now.Add(want - time.Second)
		if res, err := d.Tick(context.Background()); err != nil || res != (TickResult{}) {
			t.Fatalf("event re-claimed before its backoff elapsed: %+v err=%v", res, err)
		}
		now = now.Add(time.Second) // now it is due
	}
	if got := len(broken.delivered()); got != 3 {
		t.Fatalf("channel called %d times, want 3 (one per elapsed backoff)", got)
	}
}

// When the attempt budget runs out the event is ABANDONED: stamped, kept
// unpublished, carrying the reason — never silently dropped and never retried
// forever against a dead credential.
func TestDispatcher_AbandonsAfterMaxAttempts(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	ev := createdEvent(uuid.New())
	ev.CreatedAt = base

	outbox := newFakeOutbox(ev)
	broken := newStubNotifier(domain.ChannelWhatsApp, errors.New("meta: token expired"), domain.EventBookingCreated)
	cfg := DispatcherConfig{BatchSize: 10, RetryBaseDelay: time.Minute, RetryMaxDelay: time.Minute, MaxAttempts: 3}

	now := base
	d := NewDispatcher(outbox, noopTx{}, cfg, testLog(), broken)
	d.now = func() time.Time { return now }

	for i := 1; i <= 2; i++ {
		res, err := d.Tick(context.Background())
		if err != nil || res.Retry != 1 {
			t.Fatalf("tick %d = %+v err=%v, want one retry", i, res, err)
		}
		now = now.Add(time.Minute)
	}
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("final tick: %v", err)
	}
	if res.Abandoned != 1 || res.Retry != 0 {
		t.Fatalf("final tick = %+v, want one abandoned", res)
	}
	if !outbox.isAbandoned(ev.ID) {
		t.Fatal("exhausted event not stamped abandoned — it would be invisible in the dead letter")
	}
	if outbox.isPublished(ev.ID) {
		t.Fatal("abandoned event marked published: giving up must not look like a delivery")
	}
	if got := outbox.lastErrorOf(ev.ID); got == "" {
		t.Fatal("abandoned event carries no reason")
	}
	// And it is out of the queue: a later tick neither claims nor re-sends it.
	now = now.Add(24 * time.Hour)
	if res, err := d.Tick(context.Background()); err != nil || res != (TickResult{}) {
		t.Fatalf("abandoned event still claimed: %+v err=%v", res, err)
	}
	if got := len(broken.delivered()); got != 3 {
		t.Fatalf("channel called %d times, want exactly MaxAttempts=3", got)
	}
}

// A partial failure still retries the whole event, and the ledger still stops
// the successful channel from notifying twice — the fairness change must not
// weaken the dedupe contract.
func TestDispatcher_PartialFailureRetriesWithoutDoubleNotify(t *testing.T) {
	restaurant := uuid.New()
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: restaurant, Endpoint: "e", P256dh: "p", Auth: "a"}
	ev := createdEvent(restaurant)
	ev.CreatedAt = base

	outbox := newFakeOutbox(ev)
	sender := newRecordingSender()
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), newFakeSettings(), sender.send, true)
	broken := newStubNotifier(domain.ChannelWhatsApp, errors.New("meta: 500"), domain.EventBookingCreated)

	now := base
	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{BatchSize: 10, RetryBaseDelay: time.Minute}, testLog(), wp, broken)
	d.now = func() time.Time { return now }

	if res, err := d.Tick(context.Background()); err != nil || res.Retry != 1 {
		t.Fatalf("tick 1 = %+v err=%v, want one retry", res, err)
	}
	now = now.Add(time.Minute)
	broken.err = nil
	if res, err := d.Tick(context.Background()); err != nil || res.Dispatched != 1 {
		t.Fatalf("tick 2 = %+v err=%v, want one dispatched", res, err)
	}
	if got := len(sender.sentIDs()); got != 1 {
		t.Fatalf("web push sent %d times across the retry, want 1 (ledger dedupe)", got)
	}
}
