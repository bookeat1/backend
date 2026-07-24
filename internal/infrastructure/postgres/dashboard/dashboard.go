// Package dashboard is a read-only Postgres read model for the superadmin
// platform dashboard (Ф1). Every method is a single aggregate query
// (COUNT / GROUP BY / SUM) over the live tables — there is no dashboard table
// and nothing here ever writes. All aggregation happens in SQL, never in a Go
// loop over full tables.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository reads platform-wide aggregates for the superadmin dashboard.
type Repository struct{ pool sqltx.Querier }

// New builds the dashboard read-model repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

// Overview returns the platform top-line counters in one round-trip. The 7/30
// day booking windows are computed relative to the database's now() so they are
// always "trailing N days from server time", independent of any period param.
func (r *Repository) Overview(ctx context.Context) (domain.PlatformOverview, error) {
	var o domain.PlatformOverview
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM restaurants)                                        AS total_restaurants,
			(SELECT count(*) FROM restaurants WHERE is_active)                        AS active_restaurants,
			(SELECT count(*) FROM users)                                             AS total_users,
			(SELECT count(*) FROM bookings)                                          AS total_bookings,
			(SELECT count(*) FROM bookings WHERE created_at >= now() - interval '7 days')  AS bookings_7d,
			(SELECT count(*) FROM bookings WHERE created_at >= now() - interval '30 days') AS bookings_30d`,
	).Scan(&o.TotalRestaurants, &o.ActiveRestaurants, &o.TotalUsers,
		&o.TotalBookings, &o.BookingsLast7Days, &o.BookingsLast30Days)
	if err != nil {
		return domain.PlatformOverview{}, fmt.Errorf("dashboard overview: %w", err)
	}
	return o, nil
}

// BookingsByStatus returns booking counts grouped by status over the half-open
// period [from, to) on created_at. A status with zero bookings in the window is
// simply absent from the result (the usecase fills the full status set). The
// caller passes an already-validated, non-zero period.
func (r *Repository) BookingsByStatus(ctx context.Context, from, to any) ([]domain.BookingStatusCount, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT status, count(*)
		   FROM bookings
		  WHERE created_at >= $1 AND created_at < $2
		  GROUP BY status
		  ORDER BY status`, from, to)
	if err != nil {
		return nil, fmt.Errorf("dashboard bookings by status: %w", err)
	}
	defer rows.Close()

	var out []domain.BookingStatusCount
	for rows.Next() {
		var c domain.BookingStatusCount
		if err := rows.Scan(&c.Status, &c.Count); err != nil {
			return nil, fmt.Errorf("scan booking status count: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate booking status counts: %w", err)
	}
	return out, nil
}

// PaymentsGMV aggregates captured (GMV) and refunded money over the half-open
// period [from, to), for a single currency. Both figures are summed from the
// STORED amounts, never recomputed:
//
//   - captured = SUM(payments.amount_minor) over payments whose money was
//     actually taken from the guest at some point — captured_at within the
//     window. A payment later partially/fully refunded is still counted as
//     captured GMV (the money did flow); the refund is reported separately.
//   - refunded = SUM(payment_refunds.amount_minor) over refunds that succeeded,
//     created within the window.
//
// COALESCE turns "no rows" into 0 so an empty platform scans as zeros, not a
// NULL scan error.
func (r *Repository) PaymentsGMV(ctx context.Context, from, to any, currency string) (domain.MoneyAggregate, domain.MoneyAggregate, error) {
	var captured domain.MoneyAggregate
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COALESCE(sum(amount_minor), 0), count(*)
		   FROM payments
		  WHERE currency = $1
		    AND captured_at IS NOT NULL
		    AND captured_at >= $2 AND captured_at < $3`, currency, from, to).
		Scan(&captured.AmountMinor, &captured.Count)
	if err != nil {
		return domain.MoneyAggregate{}, domain.MoneyAggregate{}, fmt.Errorf("dashboard captured gmv: %w", err)
	}

	var refunded domain.MoneyAggregate
	err = sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COALESCE(sum(amount_minor), 0), count(*)
		   FROM payment_refunds
		  WHERE currency = $1
		    AND status = 'succeeded'
		    AND created_at >= $2 AND created_at < $3`, currency, from, to).
		Scan(&refunded.AmountMinor, &refunded.Count)
	if err != nil {
		return domain.MoneyAggregate{}, domain.MoneyAggregate{}, fmt.Errorf("dashboard refunded total: %w", err)
	}
	return captured, refunded, nil
}

// TopRestaurantsByBookings returns the restaurants with the most bookings over
// the half-open period [from, to) on bookings.created_at, most bookings first
// (id tie-break for a stable order), capped at limit. Only restaurants with at
// least one booking in the window appear.
func (r *Repository) TopRestaurantsByBookings(ctx context.Context, from, to any, limit int) ([]domain.TopRestaurant, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT r.id, r.name, r.name_i18n, count(b.id) AS bookings_count
		   FROM bookings b
		   JOIN restaurants r ON r.id = b.restaurant_id
		  WHERE b.created_at >= $1 AND b.created_at < $2
		  GROUP BY r.id, r.name, r.name_i18n
		  ORDER BY bookings_count DESC, r.id ASC
		  LIMIT $3`, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("dashboard top restaurants by bookings: %w", err)
	}
	return scanTopRestaurants(rows)
}

// TopRestaurantsByGMV returns the restaurants with the highest captured GMV
// (single currency) over the half-open period [from, to) on payments.captured_at,
// highest first (id tie-break), capped at limit. Only restaurants with captured
// money in the window appear. GMV is summed from the stored amounts.
func (r *Repository) TopRestaurantsByGMV(ctx context.Context, from, to any, currency string, limit int) ([]domain.TopRestaurant, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT r.id, r.name, r.name_i18n, COALESCE(sum(p.amount_minor), 0) AS gmv_minor
		   FROM payments p
		   JOIN restaurants r ON r.id = p.restaurant_id
		  WHERE p.currency = $1
		    AND p.captured_at IS NOT NULL
		    AND p.captured_at >= $2 AND p.captured_at < $3
		  GROUP BY r.id, r.name, r.name_i18n
		  ORDER BY gmv_minor DESC, r.id ASC
		  LIMIT $4`, currency, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("dashboard top restaurants by gmv: %w", err)
	}
	return scanTopRestaurantsGMV(rows)
}

// rowScanner is the subset of pgx.Rows the scan helpers use.
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func scanTopRestaurants(rows rowScanner) ([]domain.TopRestaurant, error) {
	defer rows.Close()
	var out []domain.TopRestaurant
	for rows.Next() {
		var t domain.TopRestaurant
		var nameI18n []byte
		if err := rows.Scan(&t.RestaurantID, &t.Name, &nameI18n, &t.BookingsCount); err != nil {
			return nil, fmt.Errorf("scan top restaurant: %w", err)
		}
		t.NameI18n = i18nFromDB(nameI18n)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top restaurants: %w", err)
	}
	return out, nil
}

func scanTopRestaurantsGMV(rows rowScanner) ([]domain.TopRestaurant, error) {
	defer rows.Close()
	var out []domain.TopRestaurant
	for rows.Next() {
		var t domain.TopRestaurant
		var nameI18n []byte
		if err := rows.Scan(&t.RestaurantID, &t.Name, &nameI18n, &t.GMVMinor); err != nil {
			return nil, fmt.Errorf("scan top restaurant gmv: %w", err)
		}
		t.NameI18n = i18nFromDB(nameI18n)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top restaurants gmv: %w", err)
	}
	return out, nil
}

// i18nFromDB decodes a jsonb name_i18n column into domain.I18n. Mirrors the
// unexported helper in the restaurant repository (kept local — an invalid/empty
// jsonb resolves to nil, and I18n.Resolve then falls back to the base name).
func i18nFromDB(b []byte) domain.I18n {
	if len(b) == 0 {
		return nil
	}
	var m domain.I18n
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
