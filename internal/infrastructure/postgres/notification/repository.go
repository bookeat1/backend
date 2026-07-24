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

func (r *Deliveries) AlreadyDelivered(ctx context.Context, outboxEventID uuid.UUID, channel domain.NotificationChannel, subscriptionID uuid.UUID) (bool, error) {
	var exists bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM notification_deliveries
		                WHERE outbox_event_id=$1 AND channel=$2 AND subscription_id=$3)`,
		outboxEventID, string(channel), subscriptionID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check notification delivery: %w", err)
	}
	return exists, nil
}

// RecordDelivered writes the delivery marker. ON CONFLICT DO NOTHING makes it
// safe under a redelivery race: a second recording of the same
// (event, channel, subscription) is a no-op, never a unique-violation error.
func (r *Deliveries) RecordDelivered(ctx context.Context, outboxEventID uuid.UUID, channel domain.NotificationChannel, subscriptionID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO notification_deliveries (id, outbox_event_id, channel, subscription_id, created_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (outbox_event_id, channel, subscription_id) DO NOTHING`,
		uuid.New(), outboxEventID, string(channel), subscriptionID); err != nil {
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
