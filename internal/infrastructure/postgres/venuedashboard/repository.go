// Package venuedashboard is the venue-scoped read model behind a restaurant's
// own dashboard.
//
// Every query here filters by restaurant_id. That predicate is the tenant
// boundary of the feature: the handler checks the caller manages the venue, but
// this layer is what makes sure the numbers actually belong to it, and the two
// checks are deliberately not the same check.
package venuedashboard

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.VenueDashboardRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the venue dashboard read model.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.VenueDashboardRepository = (*Repository)(nil)

// Summary reads one venue's counters over the half-open period [from, to) on
// created_at — the same window convention the platform dashboard uses, so the
// two never disagree about which day a booking belongs to.
func (r *Repository) Summary(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) (domain.VenueDashboard, error) {
	out := domain.VenueDashboard{From: from, To: to}
	q := sqltx.From(ctx, r.pool)

	rows, err := q.Query(ctx,
		`SELECT status, count(*), coalesce(sum(guests), 0)
		   FROM bookings
		  WHERE restaurant_id = $1 AND created_at >= $2 AND created_at < $3
		  GROUP BY status
		  ORDER BY status`, restaurantID, from, to)
	if err != nil {
		return out, fmt.Errorf("venue dashboard by status: %w", err)
	}
	defer rows.Close()

	var totalGuests int64
	var lost int64
	for rows.Next() {
		var c domain.BookingStatusCount
		var guests int64
		if err := rows.Scan(&c.Status, &c.Count, &guests); err != nil {
			return out, fmt.Errorf("scan venue status count: %w", err)
		}
		out.ByStatus = append(out.ByStatus, c)
		out.Total += c.Count
		totalGuests += guests
		if c.Status == domain.BookingCancelled || c.Status == domain.BookingNoShow {
			lost += c.Count
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate venue status counts: %w", err)
	}

	if out.Total > 0 {
		out.CancelledShare = round1(float64(lost) * 100 / float64(out.Total))
		out.AvgPartySize = round1(float64(totalGuests) / float64(out.Total))
	}

	if err := r.cancelReasons(ctx, &out, restaurantID, from, to); err != nil {
		return out, err
	}
	if err := r.preorders(ctx, &out, restaurantID, from, to); err != nil {
		return out, err
	}
	return out, nil
}

// cancelReasons groups cancellations by their reason code. A cancellation with
// no code is kept as an empty reason rather than dropped: "we do not know why"
// is itself a number the venue needs, and hiding it would make the reasons add
// up to less than the cancellations.
func (r *Repository) cancelReasons(ctx context.Context, out *domain.VenueDashboard, restaurantID uuid.UUID, from, to time.Time) error {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT coalesce(cancellation_reason_code, ''), count(*)
		   FROM bookings
		  WHERE restaurant_id = $1 AND created_at >= $2 AND created_at < $3
		    AND status = 'cancelled'
		  GROUP BY 1
		  ORDER BY 2 DESC, 1`, restaurantID, from, to)
	if err != nil {
		return fmt.Errorf("venue dashboard cancel reasons: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c domain.CancelReasonCount
		if err := rows.Scan(&c.Reason, &c.Count); err != nil {
			return fmt.Errorf("scan cancel reason: %w", err)
		}
		out.CancelReasons = append(out.CancelReasons, c)
	}
	return rows.Err()
}

// preorders counts the bookings that carried a pre-order and their value.
//
// The join is on the booking, not on the item, so a booking with five dishes
// counts once — the venue is asking "how many guests pre-order", not "how many
// dishes were listed". The value is summed in minor units and stays an integer
// the whole way.
func (r *Repository) preorders(ctx context.Context, out *domain.VenueDashboard, restaurantID uuid.UUID, from, to time.Time) error {
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*), coalesce(sum(total_minor), 0)
		   FROM (
		     SELECT bi.booking_id,
		            sum(bi.item_price_minor * bi.quantity) AS total_minor
		       FROM booking_items bi
		       JOIN bookings b ON b.id = bi.booking_id
		      WHERE b.restaurant_id = $1 AND b.created_at >= $2 AND b.created_at < $3
		      GROUP BY bi.booking_id
		   ) t`, restaurantID, from, to).Scan(&out.PreorderBookings, &out.PreorderTotalMinor)
	if err != nil {
		return fmt.Errorf("venue dashboard preorders: %w", err)
	}
	return nil
}

// Load buckets bookings by weekday and hour of the RESERVED time, not of the
// moment the booking was made: the venue is asking when its room is busy, not
// when people happen to open the app.
//
// The extract runs in the venue's own timezone. Reading it in UTC would shift
// an Almaty evening into the small hours and make the busiest slot look empty.
func (r *Repository) Load(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.VenueLoadSlot, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT extract(dow  FROM b.starts_at AT TIME ZONE coalesce(r.timezone, 'Asia/Almaty'))::int,
		        extract(hour FROM b.starts_at AT TIME ZONE coalesce(r.timezone, 'Asia/Almaty'))::int,
		        count(*), coalesce(sum(b.guests), 0)
		   FROM bookings b
		   JOIN restaurants r ON r.id = b.restaurant_id
		  WHERE b.restaurant_id = $1 AND b.starts_at >= $2 AND b.starts_at < $3
		    AND b.status <> 'cancelled'
		  GROUP BY 1, 2
		  ORDER BY 1, 2`, restaurantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("venue dashboard load: %w", err)
	}
	defer rows.Close()

	out := []domain.VenueLoadSlot{}
	for rows.Next() {
		var s domain.VenueLoadSlot
		if err := rows.Scan(&s.Weekday, &s.Hour, &s.Bookings, &s.Guests); err != nil {
			return nil, fmt.Errorf("scan venue load slot: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
