package domain

import (
	"context"

	"github.com/google/uuid"
)

// NotificationChannel names an outbound notification channel. Increment 1 ships
// only web push; Telegram / CRM / Calendar channels register additional names
// here later without touching the dispatcher.
type NotificationChannel string

const (
	ChannelWebPush NotificationChannel = "web_push"
)

// PushSubscription is a staff member's browser Web Push subscription, as handed
// to the backend by the frontend service worker (PushSubscription.toJSON()).
// It is scoped to BOTH the staff user_id (who owns the device) and a
// restaurant_id (which venue's bookings this device wants alerts for): a staff
// member working two venues registers two subscriptions from the same browser,
// one per venue, and only ever sees the bookings of a venue they are staff of.
type PushSubscription struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	RestaurantID uuid.UUID
	Endpoint     string
	P256dh       string
	Auth         string
}

// PushSubscriptionRepository persists browser push subscriptions.
type PushSubscriptionRepository interface {
	// Upsert stores a subscription, keyed on its unique endpoint: re-registering
	// the same endpoint (e.g. after the browser rotates its keys, or the same
	// device re-subscribes) overwrites the row in place instead of duplicating.
	Upsert(ctx context.Context, s *PushSubscription) error
	// DeleteByEndpointForUser removes ONE subscription, but only if it belongs to
	// userID — a staff member can only unregister their own device, never
	// someone else's endpoint. Absent / not-owned is not an error (idempotent).
	DeleteByEndpointForUser(ctx context.Context, userID uuid.UUID, endpoint string) error
	// ListByRestaurant returns every push subscription registered for a venue —
	// the fan-out target set for that venue's new-booking event.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]PushSubscription, error)
	// DeleteByID removes a subscription the push service reported as gone (HTTP
	// 404/410): the endpoint is dead, keeping it only wastes future sends.
	DeleteByID(ctx context.Context, id uuid.UUID) error
}

// NotificationDeliveryRepository is the at-least-once dedupe ledger. A row is
// written only AFTER a successful send, so a crash between send and record
// re-sends (a tolerated duplicate) rather than dropping a notification; the
// AlreadyDelivered pre-check then stops a redelivery of the same outbox event
// from re-notifying a subscription that already got it.
type NotificationDeliveryRepository interface {
	AlreadyDelivered(ctx context.Context, outboxEventID uuid.UUID, channel NotificationChannel, subscriptionID uuid.UUID) (bool, error)
	RecordDelivered(ctx context.Context, outboxEventID uuid.UUID, channel NotificationChannel, subscriptionID uuid.UUID) error
}

// RestaurantNotificationSettingsRepository backs the per-restaurant channel
// toggle. Web push defaults to ON: a MISSING settings row means enabled, so
// WebPushEnabled returns true when the venue has never touched its settings.
type RestaurantNotificationSettingsRepository interface {
	WebPushEnabled(ctx context.Context, restaurantID uuid.UUID) (bool, error)
}
