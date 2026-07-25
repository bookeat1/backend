package payout

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

// Settings implements domain.PayoutSettingsRepository over
// restaurant_payout_settings (migration 0053).
//
// The three-state semantics of the domain type map 1:1 onto SQL and are NOT
// translated here: a missing row stays ErrNotFound / an absent map key, and a
// NULL column stays a nil pointer. Resolving "what actually applies" is
// domain.PayoutSettings.Effective's job — this layer never guesses a default,
// because a default invented in SQL is a default nobody can find later.
type Settings struct{ pool sqltx.Querier }

// NewSettings builds the per-venue payout settings repository.
func NewSettings(pool sqltx.Querier) *Settings { return &Settings{pool: pool} }

var _ domain.PayoutSettingsRepository = (*Settings)(nil)

const settingsCols = `restaurant_id, min_payout_minor, max_hold_days, updated_by, created_at, updated_at`

// Get returns one venue's overrides or domain.ErrNotFound.
func (r *Settings) Get(ctx context.Context, restaurantID uuid.UUID) (*domain.PayoutSettings, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+settingsCols+` FROM restaurant_payout_settings WHERE restaurant_id=$1`,
		restaurantID)
	s, err := scanSettings(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get payout settings: %w", err)
	}
	return s, nil
}

// Upsert stores a venue's overrides in place (one row per restaurant).
//
// Every override column is written unconditionally, including to NULL: clearing
// an override IS a legitimate edit ("this venue goes back to the platform
// default"), and a COALESCE-style partial update would make it unexpressible.
func (r *Settings) Upsert(ctx context.Context, s *domain.PayoutSettings) error {
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_payout_settings (`+settingsCols+`)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (restaurant_id) DO UPDATE SET
			min_payout_minor = EXCLUDED.min_payout_minor,
			max_hold_days    = EXCLUDED.max_hold_days,
			updated_by       = EXCLUDED.updated_by,
			updated_at       = EXCLUDED.updated_at`,
		s.RestaurantID, s.MinPayoutMinor, s.MaxHoldDays, s.UpdatedBy, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return mapWrite(err, "upsert payout settings")
	}
	return nil
}

// ForRestaurants resolves overrides for many venues in ONE query. Venues with
// no row are simply absent from the map — the caller then applies the platform
// default, which keeps "this venue has no policy of its own" a visible decision
// in the usecase rather than something buried in SQL.
func (r *Settings) ForRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]domain.PayoutSettings, error) {
	out := make(map[uuid.UUID]domain.PayoutSettings, len(restaurantIDs))
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+settingsCols+` FROM restaurant_payout_settings WHERE restaurant_id = ANY($1)`,
		restaurantIDs)
	if err != nil {
		return nil, fmt.Errorf("read payout settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		s, err := scanSettings(rows)
		if err != nil {
			return nil, fmt.Errorf("scan payout settings: %w", err)
		}
		out[s.RestaurantID] = *s
	}
	return out, rows.Err()
}

func scanSettings(row scanner) (*domain.PayoutSettings, error) {
	var s domain.PayoutSettings
	if err := row.Scan(&s.RestaurantID, &s.MinPayoutMinor, &s.MaxHoldDays,
		&s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}
