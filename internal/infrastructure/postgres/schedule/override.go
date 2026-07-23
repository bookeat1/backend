// Package schedule is the Postgres implementation of
// domain.ScheduleOverrideRepository (restaurant special-day schedule overrides).
package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const checkViolation = "23514"

// Repository implements domain.ScheduleOverrideRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the schedule-override repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.ScheduleOverrideRepository = (*Repository)(nil)

func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.ScheduleOverride, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, override_date, is_closed, open_time, close_time, note, created_at, updated_at
		 FROM restaurant_schedule_overrides
		 WHERE restaurant_id=$1
		 ORDER BY override_date ASC`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list schedule overrides: %w", err)
	}
	defer rows.Close()

	var out []domain.ScheduleOverride
	for rows.Next() {
		var o domain.ScheduleOverride
		if err := rows.Scan(&o.ID, &o.RestaurantID, &o.Date, &o.IsClosed,
			&o.OpenTime, &o.CloseTime, &o.Note, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan schedule override: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedule overrides: %w", err)
	}
	return out, nil
}

// Upsert inserts or replaces the override for (restaurant_id, override_date).
// The ON CONFLICT on the unique (restaurant_id, override_date) index makes
// "set the override for this day" idempotent — a repeat call for the same day
// updates in place. A CHECK-constraint violation (is_closed/open_time/close_time
// mismatch) is mapped to ErrValidation so the caller returns 422, not 500.
func (r *Repository) Upsert(ctx context.Context, o *domain.ScheduleOverride) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_schedule_overrides
			(id, restaurant_id, override_date, is_closed, open_time, close_time, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (restaurant_id, override_date) DO UPDATE SET
			is_closed  = EXCLUDED.is_closed,
			open_time  = EXCLUDED.open_time,
			close_time = EXCLUDED.close_time,
			note       = EXCLUDED.note,
			updated_at = now()`,
		o.ID, o.RestaurantID, o.Date, o.IsClosed, o.OpenTime, o.CloseTime, o.Note)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == checkViolation {
			return fmt.Errorf("upsert schedule override: %w", domain.ErrValidation)
		}
		return fmt.Errorf("upsert schedule override: %w", err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, restaurantID uuid.UUID, date time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM restaurant_schedule_overrides WHERE restaurant_id=$1 AND override_date=$2`,
		restaurantID, date)
	if err != nil {
		return fmt.Errorf("delete schedule override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
