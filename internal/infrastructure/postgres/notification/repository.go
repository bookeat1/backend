// Package notification is the Postgres implementation of the web-push
// notification repositories (subscriptions, the per-restaurant channel toggle,
// and the at-least-once delivery dedupe ledger).
package notification

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Subscriptions implements domain.PushSubscriptionRepository.
type Subscriptions struct{ pool sqltx.Querier }

// NewSubscriptions builds the push-subscription repository.
func NewSubscriptions(pool sqltx.Querier) *Subscriptions { return &Subscriptions{pool: pool} }

var _ domain.PushSubscriptionRepository = (*Subscriptions)(nil)

const subCols = `id, user_id, restaurant_id, endpoint, p256dh, auth`

// Upsert stores a subscription keyed on its unique endpoint. A repeat
// registration of the same endpoint overwrites the owning user, restaurant and
// keys in place (a device re-subscribing after a key rotation), never a
// duplicate row.
func (r *Subscriptions) Upsert(ctx context.Context, s *domain.PushSubscription) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	q := `INSERT INTO push_subscriptions (` + subCols + `, created_at)
	      VALUES ($1,$2,$3,$4,$5,$6, now())
	      ON CONFLICT (endpoint) DO UPDATE
	        SET user_id       = EXCLUDED.user_id,
	            restaurant_id = EXCLUDED.restaurant_id,
	            p256dh        = EXCLUDED.p256dh,
	            auth          = EXCLUDED.auth`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		s.ID, s.UserID, s.RestaurantID, s.Endpoint, s.P256dh, s.Auth); err != nil {
		return fmt.Errorf("upsert push subscription: %w", err)
	}
	return nil
}

// DeleteByEndpointForUser removes the caller's own subscription by endpoint.
// The user_id predicate is the tenant guard: it is impossible to unregister
// another user's endpoint even knowing its exact value.
func (r *Subscriptions) DeleteByEndpointForUser(ctx context.Context, userID uuid.UUID, endpoint string) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM push_subscriptions WHERE user_id=$1 AND endpoint=$2`, userID, endpoint); err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

// ListByRestaurant returns every subscription registered for a venue.
func (r *Subscriptions) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.PushSubscription, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+subCols+` FROM push_subscriptions WHERE restaurant_id=$1 ORDER BY created_at`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()
	var out []domain.PushSubscription
	for rows.Next() {
		var s domain.PushSubscription
		if err := rows.Scan(&s.ID, &s.UserID, &s.RestaurantID, &s.Endpoint, &s.P256dh, &s.Auth); err != nil {
			return nil, fmt.Errorf("list push subscriptions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteByID drops a dead endpoint the push service rejected with 404/410.
func (r *Subscriptions) DeleteByID(ctx context.Context, id uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM push_subscriptions WHERE id=$1`, id); err != nil {
		return fmt.Errorf("delete push subscription by id: %w", err)
	}
	return nil
}

// Deliveries implements domain.NotificationDeliveryRepository.
type Deliveries struct{ pool sqltx.Querier }

// NewDeliveries builds the delivery-dedupe repository.
func NewDeliveries(pool sqltx.Querier) *Deliveries { return &Deliveries{pool: pool} }

var _ domain.NotificationDeliveryRepository = (*Deliveries)(nil)

func (r *Deliveries) AlreadyDelivered(ctx context.Context, outboxEventID uuid.UUID, channel domain.NotificationChannel, targetID uuid.UUID) (bool, error) {
	var exists bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM notification_deliveries
		                WHERE outbox_event_id=$1 AND channel=$2 AND target_id=$3)`,
		outboxEventID, string(channel), targetID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check notification delivery: %w", err)
	}
	return exists, nil
}

// RecordDelivered writes the delivery marker. ON CONFLICT DO NOTHING makes it
// safe under a redelivery race: a second recording of the same
// (event, channel, target) is a no-op, never a unique-violation error.
func (r *Deliveries) RecordDelivered(ctx context.Context, outboxEventID uuid.UUID, channel domain.NotificationChannel, targetID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO notification_deliveries (id, outbox_event_id, channel, target_id, created_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (outbox_event_id, channel, target_id) DO NOTHING`,
		uuid.New(), outboxEventID, string(channel), targetID); err != nil {
		return fmt.Errorf("record notification delivery: %w", err)
	}
	return nil
}

// Settings implements domain.RestaurantNotificationSettingsRepository.
type Settings struct{ pool sqltx.Querier }

// NewSettings builds the per-restaurant notification-settings repository.
func NewSettings(pool sqltx.Querier) *Settings { return &Settings{pool: pool} }

var _ domain.RestaurantNotificationSettingsRepository = (*Settings)(nil)

// WebPushEnabled reports whether web push is enabled for a venue. A missing row
// is treated as ENABLED (default on): disabling is an explicit opt-out.
func (r *Settings) WebPushEnabled(ctx context.Context, restaurantID uuid.UUID) (bool, error) {
	var enabled bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT web_push_enabled FROM restaurant_notification_settings WHERE restaurant_id=$1`,
		restaurantID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notification settings: %w", err)
	}
	return enabled, nil
}

// TelegramSettings returns the venue's telegram target + toggle. A missing row
// is TelegramSettings{ChatID: "", Enabled: true}: telegram defaults enabled but
// is silent until a chat id is connected.
func (r *Settings) TelegramSettings(ctx context.Context, restaurantID uuid.UUID) (domain.TelegramSettings, error) {
	var chatID *string
	var enabled bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT telegram_chat_id, telegram_enabled FROM restaurant_notification_settings WHERE restaurant_id=$1`,
		restaurantID).Scan(&chatID, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TelegramSettings{ChatID: "", Enabled: true}, nil
	}
	if err != nil {
		return domain.TelegramSettings{}, fmt.Errorf("read telegram settings: %w", err)
	}
	out := domain.TelegramSettings{Enabled: enabled}
	if chatID != nil {
		out.ChatID = *chatID
	}
	return out, nil
}

// RestaurantByTelegramChatID resolves a chat id back to its venue. The
// telegram_enabled predicate is part of the authorisation, not an optimisation:
// switching the channel off in the panel must also stop the buttons in messages
// that are already sitting in the chat.
//
// The chat id is unique per venue in practice (staff connect one chat), but the
// column carries no unique constraint, so LIMIT 1 keeps a duplicated row from
// turning a lookup into an error for everybody.
func (r *Settings) RestaurantByTelegramChatID(ctx context.Context, chatID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT restaurant_id FROM restaurant_notification_settings
		 WHERE telegram_chat_id = $1 AND telegram_enabled LIMIT 1`, chatID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, domain.ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve telegram chat: %w", err)
	}
	return id, nil
}

// SetTelegramChatID upserts the venue's telegram chat id and marks the channel
// enabled. The row is created on first use (web_push_enabled keeps its column
// default of true). Re-connecting a new chat id overwrites in place.
func (r *Settings) SetTelegramChatID(ctx context.Context, restaurantID uuid.UUID, chatID string) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_notification_settings (restaurant_id, telegram_chat_id, telegram_enabled, updated_at)
		 VALUES ($1, $2, true, now())
		 ON CONFLICT (restaurant_id) DO UPDATE
		   SET telegram_chat_id = EXCLUDED.telegram_chat_id,
		       telegram_enabled = true,
		       updated_at       = now()`,
		restaurantID, chatID); err != nil {
		return fmt.Errorf("set telegram chat id: %w", err)
	}
	return nil
}

// ClearTelegramChatID unsets the chat id, silencing the channel. Idempotent: a
// venue with no settings row stays without one (nothing to clear).
func (r *Settings) ClearTelegramChatID(ctx context.Context, restaurantID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurant_notification_settings
		    SET telegram_chat_id = NULL, updated_at = now()
		  WHERE restaurant_id = $1`,
		restaurantID); err != nil {
		return fmt.Errorf("clear telegram chat id: %w", err)
	}
	return nil
}

// DeviceTokens implements domain.DevicePushTokenRepository — the GUEST-facing
// mobile push tokens (device_push_tokens, migration 0049).
type DeviceTokens struct{ pool sqltx.Querier }

// NewDeviceTokens builds the guest device-token repository.
func NewDeviceTokens(pool sqltx.Querier) *DeviceTokens { return &DeviceTokens{pool: pool} }

var _ domain.DevicePushTokenRepository = (*DeviceTokens)(nil)

const deviceTokenCols = `id, user_id, token, platform, is_active, created_at, updated_at`

// Upsert stores a token keyed on the token value. A repeat registration of the
// same token RE-POINTS the row at the calling user and reactivates it, never a
// duplicate — the device may have changed hands since it was last registered.
func (r *DeviceTokens) Upsert(ctx context.Context, t *domain.DevicePushToken) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	q := `INSERT INTO device_push_tokens (` + deviceTokenCols + `)
	      VALUES ($1,$2,$3,$4, true, now(), now())
	      ON CONFLICT (token) DO UPDATE
	        SET user_id    = EXCLUDED.user_id,
	            platform   = EXCLUDED.platform,
	            is_active  = true,
	            updated_at = now()
	      RETURNING id, is_active, created_at, updated_at`
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx, q,
		t.ID, t.UserID, t.Token, string(t.Platform),
	).Scan(&t.ID, &t.IsActive, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return fmt.Errorf("upsert device push token: %w", err)
	}
	return nil
}

// ListActiveByUser returns the guest's live devices, oldest first.
func (r *DeviceTokens) ListActiveByUser(ctx context.Context, userID uuid.UUID) ([]domain.DevicePushToken, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+deviceTokenCols+` FROM device_push_tokens
		  WHERE user_id=$1 AND is_active ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list device push tokens: %w", err)
	}
	defer rows.Close()
	var out []domain.DevicePushToken
	for rows.Next() {
		var t domain.DevicePushToken
		var platform string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Token, &platform, &t.IsActive, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list device push tokens: %w", err)
		}
		t.Platform = domain.DevicePlatform(platform)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeactivateByID silences a token the provider reported as gone. The row stays
// (the delivery ledger points at its id); only the flag flips.
func (r *DeviceTokens) DeactivateByID(ctx context.Context, id uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE device_push_tokens SET is_active=false, updated_at=now()
		  WHERE id=$1 AND is_active`, id); err != nil {
		return fmt.Errorf("deactivate device push token: %w", err)
	}
	return nil
}

// DeactivateForUser is the guest's own unregister. The user_id predicate makes
// it impossible to silence another guest's device even knowing its exact token.
func (r *DeviceTokens) DeactivateForUser(ctx context.Context, userID uuid.UUID, token string) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE device_push_tokens SET is_active=false, updated_at=now()
		  WHERE user_id=$1 AND token=$2 AND is_active`, userID, token); err != nil {
		return fmt.Errorf("deactivate device push token for user: %w", err)
	}
	return nil
}

// Venues reads the venue display name a guest-facing message needs. It is a
// deliberately minimal reader, not the catalog's RestaurantRepository.GetByID:
// that one loads the whole aggregate (images, features, tags) and the notifier
// needs one string, once per event.
type Venues struct{ pool sqltx.Querier }

// NewVenues builds the venue-name reader.
func NewVenues(pool sqltx.Querier) *Venues { return &Venues{pool: pool} }

// Name returns the venue's Russian display name (the base `name` column — the
// guest-facing texts are Russian, so no locale resolution is needed here). A
// missing venue yields domain.ErrNotFound.
func (r *Venues) Name(ctx context.Context, restaurantID uuid.UUID) (string, error) {
	var name string
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT name FROM restaurants WHERE id=$1`, restaurantID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read venue name: %w", err)
	}
	return name, nil
}

// Timezone returns the IANA zone stored on the venue, or "" when it has none of
// its own (the platform default then applies — resolving that is the caller's
// job, exactly as in usecase/bookings and usecase/payouts). A missing venue
// yields domain.ErrNotFound.
//
// The column is NULLABLE, so NULL and "" are read as the same thing: "no zone
// of its own". They are NOT read as UTC — time.LoadLocation("") silently
// returns UTC, which is how a Kazakh venue ends up being told about a booking
// five hours off.
func (r *Venues) Timezone(ctx context.Context, restaurantID uuid.UUID) (string, error) {
	var tz *string
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT timezone FROM restaurants WHERE id=$1`, restaurantID).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read venue timezone: %w", err)
	}
	if tz == nil {
		return "", nil
	}
	return *tz, nil
}

// WhatsAppSettings returns the venue's WhatsApp target + toggle. A missing row
// is WhatsAppSettings{Phone: "", Enabled: true}: like Telegram, the channel
// defaults enabled but stays silent until a number is set.
func (r *Settings) WhatsAppSettings(ctx context.Context, restaurantID uuid.UUID) (domain.WhatsAppSettings, error) {
	var phone *string
	var enabled bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT whatsapp_phone, whatsapp_enabled FROM restaurant_notification_settings WHERE restaurant_id=$1`,
		restaurantID).Scan(&phone, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WhatsAppSettings{Phone: "", Enabled: true}, nil
	}
	if err != nil {
		return domain.WhatsAppSettings{}, fmt.Errorf("read whatsapp settings: %w", err)
	}
	out := domain.WhatsAppSettings{Enabled: enabled}
	if phone != nil {
		out.Phone = *phone
	}
	return out, nil
}

// RestaurantByWhatsAppPhone resolves a sender number back to its venue. The
// whatsapp_enabled predicate is part of the authorisation, not an optimisation:
// switching the channel off must also disarm the buttons in messages already
// sitting in someone's WhatsApp.
func (r *Settings) RestaurantByWhatsAppPhone(ctx context.Context, phone string) (uuid.UUID, error) {
	var id uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT restaurant_id FROM restaurant_notification_settings
		 WHERE whatsapp_phone = $1 AND whatsapp_enabled LIMIT 1`, phone).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, domain.ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve whatsapp phone: %w", err)
	}
	return id, nil
}

// SetWhatsAppPhone upserts the venue's notification number and marks the
// channel enabled. The number is stored as given by the usecase, which
// normalizes it to E.164 first — two venues must not end up with "+77010000001"
// and "87010000001" for the same phone, or the inbound lookup would miss.
func (r *Settings) SetWhatsAppPhone(ctx context.Context, restaurantID uuid.UUID, phone string) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_notification_settings (restaurant_id, whatsapp_phone, whatsapp_enabled, updated_at)
		 VALUES ($1, $2, true, now())
		 ON CONFLICT (restaurant_id) DO UPDATE
		   SET whatsapp_phone   = EXCLUDED.whatsapp_phone,
		       whatsapp_enabled = true,
		       updated_at       = now()`,
		restaurantID, phone); err != nil {
		return fmt.Errorf("set whatsapp phone: %w", err)
	}
	return nil
}

// ClearWhatsAppPhone unsets the number, silencing the channel. Idempotent.
func (r *Settings) ClearWhatsAppPhone(ctx context.Context, restaurantID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurant_notification_settings
		    SET whatsapp_phone = NULL, updated_at = now()
		  WHERE restaurant_id = $1`,
		restaurantID); err != nil {
		return fmt.Errorf("clear whatsapp phone: %w", err)
	}
	return nil
}
