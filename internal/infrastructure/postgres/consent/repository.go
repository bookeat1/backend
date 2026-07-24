// Package consent is the Postgres implementation of domain.UserConsentRepository
// and domain.UserNotificationPreferenceRepository.
package consent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// ConsentRepository persists the append-only consent log.
type ConsentRepository struct{ pool sqltx.Querier }

// NewConsentRepository builds the consent-log repository.
func NewConsentRepository(pool sqltx.Querier) *ConsentRepository {
	return &ConsentRepository{pool: pool}
}

var _ domain.UserConsentRepository = (*ConsentRepository)(nil)

// Append inserts one immutable consent record. Nothing here updates or deletes
// an existing row — history is preserved by construction.
func (r *ConsentRepository) Append(ctx context.Context, rec *domain.ConsentRecord) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO user_consents (id, user_id, consent_type, version, granted, source, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rec.ID, rec.UserID, rec.ConsentType, rec.Version, rec.Granted, string(rec.Source), rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("append consent: %w", err)
	}
	return nil
}

// CurrentState returns the latest record per consent_type for userID. DISTINCT
// ON (consent_type) with a matching ORDER BY collapses the append-only log to
// each type's newest row; the outer ORDER BY gives a stable, newest-first list.
func (r *ConsentRepository) CurrentState(ctx context.Context, userID uuid.UUID) ([]domain.ConsentRecord, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, user_id, consent_type, version, granted, source, created_at
		 FROM (
		     SELECT DISTINCT ON (consent_type)
		            id, user_id, consent_type, version, granted, source, created_at
		     FROM user_consents
		     WHERE user_id = $1
		     ORDER BY consent_type, created_at DESC, id DESC
		 ) latest
		 ORDER BY created_at DESC, consent_type`, userID)
	if err != nil {
		return nil, fmt.Errorf("consent current state: %w", err)
	}
	defer rows.Close()

	var out []domain.ConsentRecord
	for rows.Next() {
		var rec domain.ConsentRecord
		var source string
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.ConsentType, &rec.Version,
			&rec.Granted, &source, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("consent current state: %w", err)
		}
		rec.Source = domain.ConsentSource(source)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consent current state: %w", err)
	}
	return out, nil
}

// PreferenceRepository persists a guest's notification opt-out.
type PreferenceRepository struct{ pool sqltx.Querier }

// NewPreferenceRepository builds the notification-preference repository.
func NewPreferenceRepository(pool sqltx.Querier) *PreferenceRepository {
	return &PreferenceRepository{pool: pool}
}

var _ domain.UserNotificationPreferenceRepository = (*PreferenceRepository)(nil)

// Get returns the user's preference, or DefaultNotificationPreference (all
// enabled) when no row exists — a missing row is "never opted out", not an error.
func (r *PreferenceRepository) Get(ctx context.Context, userID uuid.UUID) (domain.NotificationPreference, error) {
	var p domain.NotificationPreference
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT user_id, notifications_enabled, push_enabled, email_enabled, updated_at
		 FROM user_notification_preferences WHERE user_id = $1`, userID).
		Scan(&p.UserID, &p.NotificationsEnabled, &p.PushEnabled, &p.EmailEnabled, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DefaultNotificationPreference(userID), nil
	}
	if err != nil {
		return domain.NotificationPreference{}, fmt.Errorf("get notification preference: %w", err)
	}
	return p, nil
}

// Upsert inserts or replaces the user's preference row, stamping updated_at.
func (r *PreferenceRepository) Upsert(ctx context.Context, pref domain.NotificationPreference) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO user_notification_preferences
		     (user_id, notifications_enabled, push_enabled, email_enabled, updated_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (user_id) DO UPDATE SET
		     notifications_enabled = EXCLUDED.notifications_enabled,
		     push_enabled          = EXCLUDED.push_enabled,
		     email_enabled         = EXCLUDED.email_enabled,
		     updated_at            = now()`,
		pref.UserID, pref.NotificationsEnabled, pref.PushEnabled, pref.EmailEnabled)
	if err != nil {
		return fmt.Errorf("upsert notification preference: %w", err)
	}
	return nil
}
