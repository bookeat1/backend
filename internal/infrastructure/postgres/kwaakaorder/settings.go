package kwaakaorder

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Settings implements domain.KwaakaOrderSettingsRepository.
type Settings struct{ pool sqltx.Querier }

// NewSettings builds the settings repository.
func NewSettings(pool sqltx.Querier) *Settings { return &Settings{pool: pool} }

var _ domain.KwaakaOrderSettingsRepository = (*Settings)(nil)

func (r *Settings) load(ctx context.Context, id uuid.UUID, suffix string) (*domain.KwaakaOrderSettings, error) {
	q := sqltx.From(ctx, r.pool)
	var s domain.KwaakaOrderSettings
	err := q.QueryRow(ctx,
		`SELECT restaurant_id, orders_enabled, kwaaka_restaurant_id, lead_minutes, enabled_at, updated_at, updated_by
		   FROM restaurant_kwaaka_order_settings WHERE restaurant_id = $1`+suffix, id).
		Scan(&s.RestaurantID, &s.OrdersEnabled, &s.KwaakaRestaurantID, &s.LeadMinutes, &s.EnabledAt, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("kwaaka order settings: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("kwaaka order settings: %w", err)
	}
	rows, err := q.Query(ctx,
		`SELECT kwaaka_table_id, position, label FROM restaurant_kwaaka_pool_tables
		  WHERE restaurant_id = $1 ORDER BY position, kwaaka_table_id`, id)
	if err != nil {
		return nil, fmt.Errorf("kwaaka pool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t domain.KwaakaPoolTable
		if err := rows.Scan(&t.KwaakaTableID, &t.Position, &t.Label); err != nil {
			return nil, err
		}
		s.Pool = append(s.Pool, t)
	}
	return &s, rows.Err()
}

func (r *Settings) Get(ctx context.Context, id uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	return r.load(ctx, id, "")
}

func (r *Settings) LockForUpdate(ctx context.Context, id uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	return r.load(ctx, id, " FOR UPDATE")
}

// Save upserts the settings and replaces the pool. enabled_at moves only on a
// false→true transition (or when first inserted enabled).
func (r *Settings) Save(ctx context.Context, s *domain.KwaakaOrderSettings) error {
	q := sqltx.From(ctx, r.pool)
	err := q.QueryRow(ctx,
		`INSERT INTO restaurant_kwaaka_order_settings
		   (restaurant_id, orders_enabled, kwaaka_restaurant_id, lead_minutes, enabled_at, updated_at, updated_by)
		 VALUES ($1,$2,$3,$4, CASE WHEN $2 THEN now() END, now(), $5)
		 ON CONFLICT (restaurant_id) DO UPDATE SET
		   enabled_at = CASE WHEN EXCLUDED.orders_enabled AND NOT restaurant_kwaaka_order_settings.orders_enabled
		                     THEN now() ELSE restaurant_kwaaka_order_settings.enabled_at END,
		   orders_enabled = EXCLUDED.orders_enabled,
		   kwaaka_restaurant_id = EXCLUDED.kwaaka_restaurant_id,
		   lead_minutes = EXCLUDED.lead_minutes,
		   updated_at = now(), updated_by = EXCLUDED.updated_by
		 RETURNING enabled_at, updated_at`,
		s.RestaurantID, s.OrdersEnabled, s.KwaakaRestaurantID, s.LeadMinutes, s.UpdatedBy).
		Scan(&s.EnabledAt, &s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save kwaaka order settings: %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM restaurant_kwaaka_pool_tables WHERE restaurant_id = $1`, s.RestaurantID); err != nil {
		return fmt.Errorf("clear kwaaka pool: %w", err)
	}
	for _, t := range s.Pool {
		if _, err := q.Exec(ctx,
			`INSERT INTO restaurant_kwaaka_pool_tables (restaurant_id, kwaaka_table_id, position, label)
			 VALUES ($1,$2,$3,$4)`, s.RestaurantID, t.KwaakaTableID, t.Position, t.Label); err != nil {
			return fmt.Errorf("insert kwaaka pool table: %w", err)
		}
	}
	return nil
}
