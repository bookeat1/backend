package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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

	// Repair the failing subscription and re-tick (redelivery of the same event).
	sender.errFor = map[uuid.UUID]error{}
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
