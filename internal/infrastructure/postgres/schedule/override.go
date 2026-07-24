// Package schedule is the Postgres implementation of
// domain.ScheduleOverrideRepository (restaurant special-day schedule overrides).
package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const checkViolation = "23514"

// scanner is the common Scan surface of both pgx.Row (QueryRow) and pgx.Rows
// (Query loop), so one scanOverride helper serves the single-row and the
// list paths without duplicating the column order.
type scanner interface {
	Scan(dest ...any) error
}

// scanOverride reads one row in the column order every SELECT in this file
// uses. Keeping it in one place means the two new columns (0036) were added to
// exactly one scan target, not three.
func scanOverride(s scanner) (domain.ScheduleOverride, error) {
	var o domain.ScheduleOverride
	if err := s.Scan(&o.ID, &o.RestaurantID, &o.Date, &o.IsClosed,
		&o.OpenTime, &o.CloseTime, &o.Note,
		&o.BookingPaymentRequired, &o.DepositAmountMinor,
		&o.CreatedAt, &o.UpdatedAt); err != nil {
		return domain.ScheduleOverride{}, fmt.Errorf("scan schedule override: %w", err)
	}
	return o, nil
}

// Repository implements domain.ScheduleOverrideRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the schedule-override repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.ScheduleOverrideRepository = (*Repository)(nil)

func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.ScheduleOverride, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, override_date, is_closed, open_time, close_time, note,
		        booking_payment_required, deposit_amount_minor, created_at, updated_at
		 FROM restaurant_schedule_overrides
		 WHERE restaurant_id=$1
		 ORDER BY override_date ASC`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list schedule overrides: %w", err)
	}
	defer rows.Close()

	var out []domain.ScheduleOverride
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, err
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
			(id, restaurant_id, override_date, is_closed, open_time, close_time, note,
			 booking_payment_required, deposit_amount_minor)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (restaurant_id, override_date) DO UPDATE SET
			is_closed                = EXCLUDED.is_closed,
			open_time                = EXCLUDED.open_time,
			close_time               = EXCLUDED.close_time,
			note                     = EXCLUDED.note,
			booking_payment_required = EXCLUDED.booking_payment_required,
			deposit_amount_minor     = EXCLUDED.deposit_amount_minor,
			updated_at               = now()`,
		o.ID, o.RestaurantID, o.Date, o.IsClosed, o.OpenTime, o.CloseTime, o.Note,
		o.BookingPaymentRequired, o.DepositAmountMinor)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == checkViolation {
			return fmt.Errorf("upsert schedule override: %w", domain.ErrValidation)
		}
		return fmt.Errorf("upsert schedule override: %w", err)
	}
	return nil
}

// GetForBookingInstant returns the override for the calendar date of `at` in
// the venue's own timezone. The date is derived entirely in SQL so the money
// path never has to load-and-parse the venue timezone in Go:
//
//   - `restaurants.timezone` is the venue's IANA zone (nullable, migration 0004);
//   - the LEFT JOIN on pg_timezone_names resolves it to a KNOWN zone, yielding
//     NULL for a NULL or unrecognized value, so COALESCE(z.name, $3) falls back
//     to the platform default (fallbackTZ) instead of raising on a bad row —
//     mirroring usecase/bookings.policyLocation's "never panic on a bad DB
//     value" posture;
//   - `$2 AT TIME ZONE <tz>` converts the timestamptz instant to the local
//     wall-clock timestamp in that zone, and `::date` takes its calendar day —
//     the exact value the venue stored in override_date.
//
// Returns ErrNotFound when there is no override for that local date.
func (r *Repository) GetForBookingInstant(ctx context.Context, restaurantID uuid.UUID, at time.Time, fallbackTZ string) (*domain.ScheduleOverride, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT o.id, o.restaurant_id, o.override_date, o.is_closed, o.open_time, o.close_time, o.note,
		        o.booking_payment_required, o.deposit_amount_minor, o.created_at, o.updated_at
		 FROM restaurant_schedule_overrides o
		 JOIN restaurants r ON r.id = o.restaurant_id
		 LEFT JOIN pg_timezone_names z ON z.name = r.timezone
		 WHERE o.restaurant_id = $1
		   AND o.override_date = ($2::timestamptz AT TIME ZONE COALESCE(z.name, $3::text))::date`,
		restaurantID, at, fallbackTZ)
	o, err := scanOverride(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get schedule override for instant: %w", err)
	}
	return &o, nil
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
