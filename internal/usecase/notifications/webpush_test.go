package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The web-push channel pushes ONLY to the staff subscriptions of the booking's
// own restaurant — never to another restaurant's staff (no cross-tenant push).
func TestWebPush_NoCrossTenantPush(t *testing.T) {
	restA := uuid.New()
	restB := uuid.New()
	subA := domain.PushSubscription{ID: uuid.New(), RestaurantID: restA, Endpoint: "a", P256dh: "p", Auth: "a"}
	subB := domain.PushSubscription{ID: uuid.New(), RestaurantID: restB, Endpoint: "b", P256dh: "p", Auth: "a"}

	sender := newRecordingSender()
	wp := newWebPush(newFakeSubs(subA, subB), newFakeDeliveries(), newFakeSettings(), sender.send, true)

	ev, err := toEvent(createdEvent(restA))
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if err := wp.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}

	got := sender.sentIDs()
	if len(got) != 1 || got[0] != subA.ID {
		t.Fatalf("sent = %v, want only restaurant A's subscription %s", got, subA.ID)
	}
}

// Missing VAPID keys → the notifier is disabled → Notify is a clean no-op
// (no send, no error, no crash).
func TestWebPush_MissingKeysNoOp(t *testing.T) {
	rest := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: rest, Endpoint: "a", P256dh: "p", Auth: "a"}
	sender := newRecordingSender()
	// enabled=false models absent VAPID keys.
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), newFakeSettings(), sender.send, false)

	ev, _ := toEvent(createdEvent(rest))
	if err := wp.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify with no keys should no-op, got %v", err)
	}
	if len(sender.sentIDs()) != 0 {
		t.Fatal("a push was sent despite missing VAPID keys")
	}
}

// A nil sender (also "not configured") is treated as disabled, no panic.
func TestWebPush_NilSenderNoOp(t *testing.T) {
	rest := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: rest, Endpoint: "a", P256dh: "p", Auth: "a"}
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), newFakeSettings(), nil, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := wp.Notify(context.Background(), ev); err != nil {
		t.Fatalf("nil sender should no-op, got %v", err)
	}
}

// The per-restaurant toggle: web push disabled for a venue → no send.
func TestWebPush_RestaurantToggleDisabled(t *testing.T) {
	rest := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: rest, Endpoint: "a", P256dh: "p", Auth: "a"}
	sender := newRecordingSender()
	settings := newFakeSettings()
	settings.disabled[rest] = true
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), settings, sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := wp.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(sender.sentIDs()) != 0 {
		t.Fatal("a push was sent for a restaurant with web push disabled")
	}
}

// A subscription the push service reports as gone (410) is deleted and does not
// fail the event (it must not block MarkPublished).
func TestWebPush_GoneSubscriptionDeletedNotRetried(t *testing.T) {
	rest := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: rest, Endpoint: "a", P256dh: "p", Auth: "a"}
	sender := newRecordingSender()
	sender.status[sub.ID] = 410
	subs := newFakeSubs(sub)
	wp := newWebPush(subs, newFakeDeliveries(), newFakeSettings(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := wp.Notify(context.Background(), ev); err != nil {
		t.Fatalf("a gone subscription must not be a retryable error, got %v", err)
	}
	if subs.has(sub.ID) {
		t.Fatal("gone (410) subscription was not deleted")
	}
}

// A transient push-service error (5xx) is a retryable failure: Notify returns
// an error so the dispatcher leaves the event unpublished.
func TestWebPush_TransientStatusIsRetryable(t *testing.T) {
	rest := uuid.New()
	sub := domain.PushSubscription{ID: uuid.New(), RestaurantID: rest, Endpoint: "a", P256dh: "p", Auth: "a"}
	sender := newRecordingSender()
	sender.status[sub.ID] = 503
	wp := newWebPush(newFakeSubs(sub), newFakeDeliveries(), newFakeSettings(), sender.send, true)

	ev, _ := toEvent(createdEvent(rest))
	if err := wp.Notify(context.Background(), ev); err == nil {
		t.Fatal("a 503 must surface as a retryable error, got nil")
	}
}
