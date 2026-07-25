package booking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Capacity implements domain.BookingCapacityRepository — the table-less half of
// the occupancy layer (migration 0054).
//
// This repository only ever writes booking_capacity_holds. The aggregated
// counter in restaurant_capacity_buckets, and with it the overbooking refusal,
// is maintained by the DB trigger; `active` is likewise the trigger's business
// alone, exactly as in Tables.
type Capacity struct{ pool sqltx.Querier }

// NewCapacity builds the booking capacity-hold repository.
func NewCapacity(pool sqltx.Querier) *Capacity { return &Capacity{pool: pool} }

var _ domain.BookingCapacityRepository = (*Capacity)(nil)

const capacityHoldCols = `id, booking_id, restaurant_id, bucket_start, seats, seats_limit, active, created_at`

func (r *Capacity) Create(ctx context.Context, holds []domain.BookingCapacityHold) error {
	if len(holds) == 0 {
		return nil
	}
	q, args := insertHolds(holds)
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...); err != nil {
		return mapCapacityWrite(err, "create capacity holds")
	}
	return nil
}

func (r *Capacity) ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, holds []domain.BookingCapacityHold) error {
	// DELETE + INSERT are two statements: call inside a TxManager, otherwise a
	// rejected insert leaves the booking holding no capacity at all while it
	// still occupies a seat in the venue.
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM booking_capacity_holds WHERE booking_id=$1`, bookingID); err != nil {
		return fmt.Errorf("replace capacity holds: %w", err)
	}
	for i := range holds {
		holds[i].BookingID = bookingID
	}
	return r.Create(ctx, holds)
}

func (r *Capacity) ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingCapacityHold, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+capacityHoldCols+` FROM booking_capacity_holds
		 WHERE booking_id=$1 ORDER BY bucket_start`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list capacity holds: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingCapacityHold
	for rows.Next() {
		var h domain.BookingCapacityHold
		if err := rows.Scan(&h.ID, &h.BookingID, &h.RestaurantID, &h.BucketStart,
			&h.Seats, &h.SeatsLimit, &h.Active, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("list capacity holds: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r *Capacity) ListUsage(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.CapacityUsage, error) {
	// Half-open [from, to) to match every other occupancy query in this
	// package: a bucket starting exactly at `to` belongs to the next window.
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT bucket_start, seats_taken, seats_limit
		 FROM restaurant_capacity_buckets
		 WHERE restaurant_id=$1 AND bucket_start >= $2 AND bucket_start < $3
		   AND seats_taken > 0
		 ORDER BY bucket_start`, restaurantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list capacity usage: %w", err)
	}
	defer rows.Close()
	var out []domain.CapacityUsage
	for rows.Next() {
		var u domain.CapacityUsage
		if err := rows.Scan(&u.BucketStart, &u.SeatsTaken, &u.SeatsLimit); err != nil {
			return nil, fmt.Errorf("list capacity usage: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *Capacity) PeakTaken(ctx context.Context, restaurantID uuid.UUID, from time.Time) (*domain.CapacityUsage, error) {
	var u domain.CapacityUsage
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT bucket_start, seats_taken, seats_limit
		 FROM restaurant_capacity_buckets
		 WHERE restaurant_id=$1 AND bucket_start >= $2 AND seats_taken > 0
		 ORDER BY seats_taken DESC, bucket_start
		 LIMIT 1`, restaurantID, from).Scan(&u.BucketStart, &u.SeatsTaken, &u.SeatsLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peak capacity usage: %w", err)
	}
	return &u, nil
}

// insertHolds builds one multi-row INSERT, rows sorted by bucket_start.
//
// The sort is not cosmetic: the AFTER ROW trigger fires in insertion order and
// takes a row lock on each bucket, so a fixed global order is what keeps two
// concurrent bookings over overlapping windows from deadlocking. Sorting here
// rather than trusting the caller means no caller can reintroduce the deadlock.
// The caller's slice is sorted and its generated ids backfilled in place, the
// same contract Tables.Create has.
func insertHolds(holds []domain.BookingCapacityHold) (string, []any) {
	sorted := holds
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].BucketStart.Before(sorted[j].BucketStart) })

	var sb strings.Builder
	sb.WriteString(`INSERT INTO booking_capacity_holds
		(id, booking_id, restaurant_id, bucket_start, seats, seats_limit, created_at) VALUES `)
	args := make([]any, 0, len(sorted)*7)
	now := time.Now()
	for i := range sorted {
		if sorted[i].ID == uuid.Nil {
			sorted[i].ID = uuid.New()
		}
		if sorted[i].CreatedAt.IsZero() {
			sorted[i].CreatedAt = now
		}
		n := len(args)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7)
		args = append(args, sorted[i].ID, sorted[i].BookingID, sorted[i].RestaurantID,
			sorted[i].BucketStart, sorted[i].Seats, sorted[i].SeatsLimit, sorted[i].CreatedAt)
	}
	return sb.String(), args
}
