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
	ChannelWebPush  NotificationChannel = "web_push"
	ChannelTelegram NotificationChannel = "telegram"
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
// from re-notifying the same target that already got it.
//
// targetID is the channel-specific fan-out target: a push subscription id for
// web push, the restaurant id for telegram (a venue has one chat). The ledger's
// (outbox_event_id, channel, target_id) unique key makes the dedupe scoped per
// channel, so the same booking event can still notify web push AND telegram.
type NotificationDeliveryRepository interface {
	AlreadyDelivered(ctx context.Context, outboxEventID uuid.UUID, channel NotificationChannel, targetID uuid.UUID) (bool, error)
	RecordDelivered(ctx context.Context, outboxEventID uuid.UUID, channel NotificationChannel, targetID uuid.UUID) error
}

// TelegramSettings is a venue's Telegram channel state: the chat id staff
// connected (empty when unset) and whether the channel is enabled. The notifier
// sends only when Enabled is true AND ChatID is non-empty.
type TelegramSettings struct {
	ChatID  string
	Enabled bool
}

// RestaurantNotificationSettingsRepository backs the per-restaurant channel
// toggles. Web push defaults to ON: a MISSING settings row means enabled, so
// WebPushEnabled returns true when the venue has never touched its settings.
// Telegram defaults to enabled too, but has no target until a chat id is
// connected, so a missing row leaves the telegram channel silent by default.
type RestaurantNotificationSettingsRepository interface {
	WebPushEnabled(ctx context.Context, restaurantID uuid.UUID) (bool, error)
	// TelegramSettings returns the venue's telegram target + toggle. A missing
	// settings row is TelegramSettings{ChatID: "", Enabled: true}.
	TelegramSettings(ctx context.Context, restaurantID uuid.UUID) (TelegramSettings, error)
	// SetTelegramChatID upserts the venue's telegram chat id (creating the
	// settings row on first use) and marks the channel enabled.
	SetTelegramChatID(ctx context.Context, restaurantID uuid.UUID, chatID string) error
	// ClearTelegramChatID unsets the chat id, silencing the channel while
	// preserving the rest of the venue's notification settings.
	ClearTelegramChatID(ctx context.Context, restaurantID uuid.UUID) error
}
