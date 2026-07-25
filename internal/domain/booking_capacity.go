package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CapacityMode decides HOW a venue's availability is computed, stored as
// VARCHAR on restaurants.booking_capacity_mode (NULL = CapacityModeTables).
//
//   - CapacityModeTables — the venue keeps a table list; a booking is seated at
//     specific restaurant_tables rows and the GiST exclusion constraint on
//     booking_tables is what prevents a double booking. This is how the product
//     behaved before migration 0054 and stays the default.
//   - CapacityModeSeats — the venue declares a total number of guests it can
//     seat at once and keeps no table list at all. Availability compares the
//     party against the seats still free in the requested window, and staff
//     assign the actual table themselves — exactly like the legacy product,
//     where a booking was a request rather than a seat assignment.
type CapacityMode string

const (
	CapacityModeTables CapacityMode = "tables"
	CapacityModeSeats  CapacityMode = "seats"
)

// Valid reports whether m is a known capacity mode.
func (m CapacityMode) Valid() bool {
	return m == CapacityModeTables || m == CapacityModeSeats
}

// CapacityBucket is the time granularity capacity is accounted in. It must stay
// in sync with migration 0054 — the bucket_start values written by Go are the
// primary key of restaurant_capacity_buckets, so a different step here would
// silently split one venue's occupancy across two incompatible grids.
//
// 15 minutes because every real timezone offset is a whole multiple of it (so
// UTC-floored buckets align with local quarter-hours), and because a two-hour
// visit then costs about ten rows.
const CapacityBucket = 15 * time.Minute

// BookingCapacityHold is one booking's claim on one 15-minute bucket of a
// venue's declared capacity. It is the capacity-mode counterpart of
// BookingTable.
//
// SeatsLimit is the venue's declared capacity AS OF the moment the hold was
// written: the DB CHECK that refuses an overbooking lives on the aggregated
// bucket row and cannot read the venue, so the limit travels with the hold.
//
// Active is written exclusively by the trigger on bookings.status (migration
// 0054); never set it from application code. Flipping it releases or re-claims
// the seats through the bucket trigger.
type BookingCapacityHold struct {
	ID           uuid.UUID
	BookingID    uuid.UUID
	RestaurantID uuid.UUID
	BucketStart  time.Time
	Seats        int
	SeatsLimit   int
	Active       bool
	CreatedAt    time.Time
}

// CapacityUsage is the seats already sold in one bucket, used by the
// availability engine the way TableBusyInterval is used in table mode.
type CapacityUsage struct {
	BucketStart time.Time
	SeatsTaken  int
	SeatsLimit  int
}

// Free returns the seats still sellable in the bucket, never negative. A
// negative difference is possible only right after a venue lowered its declared
// capacity below what it had already sold; the honest answer then is "nothing
// left", not a negative number.
func (u CapacityUsage) Free() int {
	if u.SeatsTaken >= u.SeatsLimit {
		return 0
	}
	return u.SeatsLimit - u.SeatsTaken
}

// BookingCapacityRepository persists capacity holds. Every write goes through
// the DB trigger that maintains restaurant_capacity_buckets, so an overbooking
// surfaces here as ErrAlreadyExists — the same shape a lost race for a table
// has in table mode.
type BookingCapacityRepository interface {
	// Create inserts the holds of one booking. Holds MUST be ordered by
	// BucketStart (the repository enforces it) so concurrent bookings lock the
	// bucket rows in the same order and cannot deadlock. Returns
	// ErrAlreadyExists when the venue's capacity is already taken.
	Create(ctx context.Context, holds []BookingCapacityHold) error
	// ReplaceForBooking deletes the booking's holds and inserts the given set
	// (call inside a TxManager). Same conflict semantics as Create.
	ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, holds []BookingCapacityHold) error
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingCapacityHold, error)
	// ListUsage returns the occupied buckets of a venue whose start lies in
	// [from, to). Buckets with no row are simply absent (nothing sold yet).
	ListUsage(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]CapacityUsage, error)
	// PeakTaken returns the busiest bucket at or after `from`, or nil when the
	// venue has nothing sold in the future. Used to refuse a capacity change
	// that would strand bookings the venue has already accepted.
	PeakTaken(ctx context.Context, restaurantID uuid.UUID, from time.Time) (*CapacityUsage, error)
}
