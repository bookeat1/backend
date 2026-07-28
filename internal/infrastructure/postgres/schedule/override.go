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

// ListByRestaurant returns EVERY override a venue has, unbounded, ordered by
// date.
//
// The lack of a date bound is deliberate and must stay: the only caller is the
// admin cabinet (usecase/admin.GetSchedule), which shows the venue the special
// days it has entered so it can edit or delete them — including the ones that
// have already passed. A window here would make rows disappear from the screen
// they are managed on.
//
// The booking engine deliberately does NOT use this method. It asks about one
// date at a time and reads through ListByRestaurantBetween, so the growth of
// this table costs an availability request nothing. If you came here to add a
// bound because this query looked unbounded, that is the method you want.
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

// dateParam renders a Go time as the bare calendar date it carries in its OWN
// location, for binding to a `date` column as text.
//
// It is deliberately not a timestamptz parameter: `$1::timestamptz::date` is
// resolved with the SESSION's TimeZone setting, so the same instant would land
// on different days on a UTC connection and an Asia/Almaty one. A "YYYY-MM-DD"
// string cast to date has exactly one meaning everywhere.
func dateParam(t time.Time) string { return t.Format("2006-01-02") }

// ListForVenues returns the overrides of MANY venues whose override_date falls
// inside [from, to] (inclusive, calendar dates), grouped by restaurant. It is
// the batch read behind the public catalog: one page of venues costs one
// round-trip, and the date bound keeps the payload from growing with every
// holiday a venue has ever had.
//
// A venue with no override in the window is simply absent from the map. The
// caller decides which venue-local dates it needs; because venues sit in
// different timezones, `from`/`to` are expected to be widened by a day on each
// side and the exact per-venue date filtering happens in the domain
// (domain.BuildScheduleExceptions).
func (r *Repository) ListForVenues(ctx context.Context, restaurantIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error) {
	out := make(map[uuid.UUID][]domain.ScheduleOverride, len(restaurantIDs))
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, override_date, is_closed, open_time, close_time, note,
		        booking_payment_required, deposit_amount_minor, created_at, updated_at
		 FROM restaurant_schedule_overrides
		 WHERE restaurant_id = ANY($1)
		   AND override_date BETWEEN $2::date AND $3::date
		 ORDER BY restaurant_id, override_date ASC`,
		restaurantIDs, dateParam(from), dateParam(to))
	if err != nil {
		return nil, fmt.Errorf("list schedule overrides for venues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, err
		}
		out[o.RestaurantID] = append(out[o.RestaurantID], o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedule overrides for venues: %w", err)
	}
	return out, nil
}

// ListByRestaurantBetween returns ONE venue's overrides whose override_date
// falls inside [from, to] (inclusive, calendar dates), ordered by date.
//
// It is the read behind the booking engine (usecase/bookings.loadSchedule),
// which is called on every availability, create and update request. The engine
// resolves a single date at a time (domain.FindScheduleOverride matches by
// exact calendar date), so it never needs a row outside a couple of days around
// the date it was asked about — while the unbounded ListByRestaurant grows for
// as long as the venue keeps entering holidays, forever, on the hottest read
// path in the service.
//
// The window is anchored on the REQUESTED date, not on today, so availability
// for a date in the past keeps behaving exactly like one in the future: it gets
// its own override, and a past closure still sells nothing. That is the
// property the previous author was protecting by leaving the query unbounded,
// and it survives — bounding relative to now would have broken it.
//
// from/to are rendered as bare calendar dates in the location they carry, so
// the caller is expected to widen them by a day or two (see
// bookings.overrideLookaround) rather than pass the exact instants: venues sit
// in zones up to 14 hours off UTC, and the engine also consults the PREVIOUS
// day for a window that runs past midnight.
func (r *Repository) ListByRestaurantBetween(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.ScheduleOverride, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, override_date, is_closed, open_time, close_time, note,
		        booking_payment_required, deposit_amount_minor, created_at, updated_at
		 FROM restaurant_schedule_overrides
		 WHERE restaurant_id=$1
		   AND override_date BETWEEN $2::date AND $3::date
		 ORDER BY override_date ASC`, restaurantID, dateParam(from), dateParam(to))
	if err != nil {
		return nil, fmt.Errorf("list schedule overrides in window: %w", err)
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
		return nil, fmt.Errorf("list schedule overrides in window: %w", err)
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
//   - the LEFT JOIN on pg_timezone_names resolves it to a zone Postgres knows,
//     under the SAME rule domain.NormalizeVenueTimezone applies in Go: an
//     "Area/Location" name, or the single exception "UTC". An abbreviation
//     ("EST", "MET") is deliberately NOT accepted even though Postgres has an
//     entry for it — those are fixed-offset compatibility zones that stay on
//     standard time all year;
//   - `$2 AT TIME ZONE <tz>` converts the timestamptz instant to the local
//     wall-clock timestamp in that zone, and `::date` takes its calendar day —
//     the exact value the venue stored in override_date.
//
// A venue with NO zone of its own (NULL/empty) uses fallbackTZ: that is the
// documented platform default, not a guess. A venue whose stored zone is
// unusable returns an error tagged CodeVenueTimezoneInvalid instead — this call
// decides whether a booking must be PREPAID and how much (usecase/payments'
// paid-special-day resolver), so the wrong calendar date here means charging a
// deposit for the wrong day, or letting a paid day go free. It used to fall
// back silently: a venue 3 hours away from the platform zone would get the
// wrong answer for every booking in those 3 hours, every day, with nothing in
// the logs.
//
// Returns ErrNotFound when there is no override for that local date.
func (r *Repository) GetForBookingInstant(ctx context.Context, restaurantID uuid.UUID, at time.Time, fallbackTZ string) (*domain.ScheduleOverride, error) {
	// One statement, one round trip: the venue row is read as a CTE and the
	// override is LEFT JOINed to it, so an unusable zone is reported even when
	// the venue has no override for that date at all — the difference between
	// "this day is free" and "we cannot tell which day this is" must not depend
	// on whether a row happens to exist.
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`WITH v AS (
		     SELECT r.timezone AS stored, z.name AS known
		     FROM restaurants r
		     LEFT JOIN pg_timezone_names z
		            ON z.name = r.timezone
		           AND (r.timezone = 'UTC' OR r.timezone LIKE '%/%')
		     WHERE r.id = $1
		 )
		 SELECT (v.stored IS NOT NULL AND v.stored <> '' AND v.known IS NULL) AS tz_unusable,
		        v.stored,
		        o.id, o.restaurant_id, o.override_date, o.is_closed, o.open_time, o.close_time, o.note,
		        o.booking_payment_required, o.deposit_amount_minor, o.created_at, o.updated_at
		 FROM v
		 LEFT JOIN restaurant_schedule_overrides o
		        ON o.restaurant_id = $1
		       AND o.override_date = ($2::timestamptz AT TIME ZONE COALESCE(v.known, $3::text))::date`,
		restaurantID, at, fallbackTZ)

	// Every override column is scanned through a pointer: the LEFT JOIN yields a
	// row for the venue even when it has no override that day, and then all of
	// them are NULL.
	var (
		tzUnusable bool
		stored     *string
		id, rid    *uuid.UUID
		date       *time.Time
		isClosed   *bool
		openTime   *string
		closeTime  *string
		note       *string
		paid       *bool
		deposit    *int64
		createdAt  *time.Time
		updatedAt  *time.Time
	)
	err := row.Scan(&tzUnusable, &stored, &id, &rid, &date, &isClosed,
		&openTime, &closeTime, &note, &paid, &deposit, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound // no such venue
		}
		return nil, fmt.Errorf("get schedule override for instant: %w", err)
	}
	if tzUnusable {
		name := ""
		if stored != nil {
			name = *stored
		}
		return nil, domain.WithCode(domain.CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: venue %s has an unusable timezone %q, so its local date cannot be determined",
				domain.ErrValidation, restaurantID, name))
	}
	if id == nil {
		return nil, domain.ErrNotFound // venue exists, no override on that local date
	}
	o := domain.ScheduleOverride{
		ID: *id, RestaurantID: *rid, Date: *date, IsClosed: *isClosed,
		OpenTime: openTime, CloseTime: closeTime, Note: note,
		BookingPaymentRequired: *paid, DepositAmountMinor: deposit,
		CreatedAt: *createdAt, UpdatedAt: *updatedAt,
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
