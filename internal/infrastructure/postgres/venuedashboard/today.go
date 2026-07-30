package venuedashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// TodayRepository implements domain.VenueTodayRepository — the operational half
// of the panel's home screen ("what do I do next"), as opposed to the counters
// in Repository ("how did we do").
//
// Like every query in this package it filters by restaurant_id, and that
// predicate is the tenant boundary: the handler proves the caller manages the
// venue, this makes sure the rows belong to it.
type TodayRepository struct{ pool sqltx.Querier }

// NewToday builds the operational read model.
func NewToday(pool sqltx.Querier) *TodayRepository { return &TodayRepository{pool: pool} }

var _ domain.VenueTodayRepository = (*TodayRepository)(nil)

// waitingMinutes is the whole minutes a row has been on the screen's clock.
// greatest(0, …) is not cosmetic: an imported booking with a created_at in the
// future would otherwise render as a negative wait, which reads like a bug to
// the hostess and hides a real one from us.
const waitingMinutes = `greatest(0, floor(extract(epoch FROM ($2::timestamptz - b.created_at)) / 60))::int`

// todayColumns is the row shape both queries scan, kept in one place so the two
// cannot drift apart.
const todayColumns = `b.id, b.starts_at, b.name, b.phone, b.guests, b.status, b.created_at, ` + waitingMinutes

// Today reads the venue's awaiting queue and its current local day.
//
// Two queries, one per list, and no N+1: the totals and the head count come
// back as window functions on the same scan. count(*) OVER () and
// sum(guests) OVER () are evaluated BEFORE the LIMIT, which is exactly what we
// need — the list is truncated for the screen, the numbers next to it are not.
func (r *TodayRepository) Today(ctx context.Context, restaurantID uuid.UUID, now time.Time,
	awaitingLimit, todayLimit int) (domain.VenueToday, error) {

	out := domain.VenueToday{Awaiting: []domain.VenueTodayBooking{}, Today: []domain.VenueTodayBooking{}}

	if err := r.awaiting(ctx, &out, restaurantID, now, awaitingLimit); err != nil {
		return domain.VenueToday{}, err
	}
	if err := r.today(ctx, &out, restaurantID, now, todayLimit); err != nil {
		return domain.VenueToday{}, err
	}
	return out, nil
}

// awaiting lists the requests the venue has not answered, OLDEST FIRST.
//
// Two decisions worth stating:
//   - Only 'pending'. A waitlisted booking has been answered — badly for the
//     guest, but answered — and putting it back in the "needs an answer" block
//     would make the block never empty.
//   - No date filter. A request for next Saturday still needs an answer today,
//     so the queue is ordered by when the request ARRIVED, not by when the
//     guest wants to come. Requests whose visit window has passed unanswered
//     are closed by the background worker (see domain.bookingTransitions), so
//     this list does not grow forever on its own.
func (r *TodayRepository) awaiting(ctx context.Context, out *domain.VenueToday,
	restaurantID uuid.UUID, now time.Time, limit int) error {

	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+todayColumns+`, count(*) OVER ()::int
		   FROM bookings b
		  WHERE b.restaurant_id = $1 AND b.status = 'pending'
		  ORDER BY b.created_at, b.id
		  LIMIT $3`, restaurantID, now, limit)
	if err != nil {
		return fmt.Errorf("venue today awaiting: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var b domain.VenueTodayBooking
		if err := rows.Scan(&b.ID, &b.StartsAt, &b.Name, &b.Phone, &b.Guests,
			&b.Status, &b.CreatedAt, &b.WaitingMinutes, &out.AwaitingTotal); err != nil {
			return fmt.Errorf("scan awaiting booking: %w", err)
		}
		out.Awaiting = append(out.Awaiting, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate awaiting bookings: %w", err)
	}
	return nil
}

// today lists the venue's current LOCAL day in time order.
//
// "Today" is the venue's calendar date, not the server's: read in UTC, an
// Almaty service that runs to 01:00 local would spill onto tomorrow's screen
// and the 05:00-to-05:00 window would cut the evening in half. The comparison
// is the same one Load uses — starts_at AT TIME ZONE coalesce(r.timezone,
// 'Asia/Almaty') — deliberately, so the two screens never disagree about which
// day a booking belongs to.
//
// Known limit, shared with Load and stated here so it is not rediscovered as a
// surprise: a restaurants.timezone holding a value Postgres cannot read (an
// empty string, a typo) makes AT TIME ZONE raise, and this endpoint fails
// rather than silently answering in the wrong zone. That is the intended side
// of the trade (see the venue-timezone rule: "no zone" and "unreadable zone"
// must not be the same branch), but it is a 500, not a validation error.
//
// Cancelled bookings are left out: they are not work. No-shows and completed
// ones stay — the day's list is what the room did, not only what is still ahead.
func (r *TodayRepository) today(ctx context.Context, out *domain.VenueToday,
	restaurantID uuid.UUID, now time.Time, limit int) error {

	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+todayColumns+`, count(*) OVER ()::int, coalesce(sum(b.guests) OVER (), 0)::int
		   FROM bookings b
		   JOIN restaurants r ON r.id = b.restaurant_id
		  WHERE b.restaurant_id = $1
		    AND b.status <> 'cancelled'
		    AND (b.starts_at AT TIME ZONE coalesce(r.timezone, 'Asia/Almaty'))::date
		      = ($2::timestamptz AT TIME ZONE coalesce(r.timezone, 'Asia/Almaty'))::date
		  ORDER BY b.starts_at, b.id
		  LIMIT $3`, restaurantID, now, limit)
	if err != nil {
		return fmt.Errorf("venue today bookings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var b domain.VenueTodayBooking
		if err := rows.Scan(&b.ID, &b.StartsAt, &b.Name, &b.Phone, &b.Guests,
			&b.Status, &b.CreatedAt, &b.WaitingMinutes, &out.TodayTotal, &out.Guests); err != nil {
			return fmt.Errorf("scan today booking: %w", err)
		}
		out.Today = append(out.Today, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate today bookings: %w", err)
	}
	return nil
}
