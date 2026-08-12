package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// NotificationChannel names an outbound notification channel. Increment 1 ships
// only web push; Telegram / CRM / Calendar channels register additional names
// here later without touching the dispatcher.
type NotificationChannel string

const (
	ChannelWebPush  NotificationChannel = "web_push"
	ChannelTelegram NotificationChannel = "telegram"
	// ChannelMobilePush is the GUEST-facing channel: a push to the signed-in
	// guest's own phone (device_push_tokens). Unlike the two above it is not a
	// staff alert, so every send through it must first pass the guest's opt-out
	// (see notifications.GuestNotificationGate).
	ChannelMobilePush NotificationChannel = "mobile_push"
	// ChannelInApp is the GUEST-facing DURABLE feed: instead of firing a message
	// at a device it appends a row to the notifications table the mobile app's
	// «Уведомления» screen reads back. It shares the dispatcher and the booking
	// outbox with the push channels but not the notification_deliveries ledger —
	// its idempotency is the notifications table's own (outbox_event_id, user_id)
	// unique key.
	ChannelInApp NotificationChannel = "in_app"
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

// DevicePlatform names the mobile platform a push token belongs to. Stored as
// VARCHAR and validated in app code — never a DB enum.
type DevicePlatform string

const (
	PlatformIOS     DevicePlatform = "ios"
	PlatformAndroid DevicePlatform = "android"
	// PlatformWeb covers an Expo web build. It is a distinct concept from the
	// staff PushSubscription (browser Web Push): this is still a token handed
	// to the same provider, not a VAPID-encrypted endpoint.
	PlatformWeb DevicePlatform = "web"
)

// ValidDevicePlatform reports whether p is a known device platform.
func ValidDevicePlatform(p DevicePlatform) bool {
	return p == PlatformIOS || p == PlatformAndroid || p == PlatformWeb
}

// DevicePushToken is ONE mobile device a signed-in guest wants notifications
// on. The app is Expo/React Native, so Token is an Expo push token in practice,
// but nothing here assumes that: the value is opaque to the domain, so an
// FCM/APNs token fits the same row once the sender behind
// notifications.MobilePushSender is swapped.
//
// It is scoped to the guest ONLY — no restaurant_id. A staff PushSubscription
// answers "alert this device about THIS venue's bookings"; a guest device is
// notified about the guest's OWN bookings, wherever they booked. One guest may
// hold several rows (phone + tablet); the unique key is the token itself.
type DevicePushToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Token     string
	Platform  DevicePlatform
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DevicePushTokenRepository persists guest mobile push tokens.
type DevicePushTokenRepository interface {
	// Upsert stores a token keyed on the token value itself. Re-registering an
	// existing token RE-POINTS it to the calling user and reactivates it,
	// instead of inserting a duplicate — a device changing hands (or a guest
	// signing in on a friend's phone) must never keep delivering to the
	// previous owner.
	Upsert(ctx context.Context, t *DevicePushToken) error
	// ListActiveByUser returns the guest's live devices — the fan-out target set
	// for their own booking events. Deactivated rows are never returned.
	ListActiveByUser(ctx context.Context, userID uuid.UUID) ([]DevicePushToken, error)
	// DeactivateByID marks a token the push provider reported as gone
	// (Expo "DeviceNotRegistered") inactive. The row is kept, not deleted: the
	// delivery ledger references it as a target. Idempotent.
	DeactivateByID(ctx context.Context, id uuid.UUID) error
	// DeactivateForUser is the guest's own "stop notifying this device" (sign
	// out / permission revoked in the OS). The userID predicate is the tenant
	// guard: it is impossible to silence someone else's device even knowing its
	// exact token. Absent / not-owned is not an error (idempotent).
	DeactivateForUser(ctx context.Context, userID uuid.UUID, token string) error
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

// WhatsAppSettings is a venue's WhatsApp channel state: the number staff
// asked us to notify (empty when unset) and whether the channel is enabled.
// Deliberately NOT the venue's public phone: that number is for guests, while
// this one may be the manager's personal number, and merging the two would one
// day show a guest a private number.
type WhatsAppSettings struct {
	Phone   string
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
	// RestaurantByTelegramChatID resolves a chat back to the venue that
	// connected it. This is the reverse of SetTelegramChatID and it is what
	// AUTHORISES an inbound button press: the press carries no account, so the
	// chat it came from is the only credential, and this lookup turns that
	// credential into a restaurant. Only an ENABLED channel resolves — a venue
	// that switched Telegram off must not still be able to act through it.
	// ErrNotFound when no venue owns the chat.
	RestaurantByTelegramChatID(ctx context.Context, chatID string) (uuid.UUID, error)

	// WhatsAppSettings mirrors TelegramSettings for the WhatsApp channel: a
	// missing row is WhatsAppSettings{Phone: "", Enabled: true} — enabled, but
	// silent until a number is set.
	WhatsAppSettings(ctx context.Context, restaurantID uuid.UUID) (WhatsAppSettings, error)
	// SetWhatsAppPhone upserts the venue's notification number (E.164) and marks
	// the channel enabled.
	SetWhatsAppPhone(ctx context.Context, restaurantID uuid.UUID, phone string) error
	// ClearWhatsAppPhone unsets the number, silencing the channel.
	ClearWhatsAppPhone(ctx context.Context, restaurantID uuid.UUID) error
	// RestaurantByWhatsAppPhone is what AUTHORISES an inbound button press from
	// WhatsApp, exactly as RestaurantByTelegramChatID does for Telegram: the
	// press carries no account, so the number it came from is the only
	// credential. Only an ENABLED channel resolves. ErrNotFound when no venue
	// owns the number.
	RestaurantByWhatsAppPhone(ctx context.Context, phone string) (uuid.UUID, error)
}
