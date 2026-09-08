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

// FreeUnder is Free capped by the venue's CURRENT declared capacity, and it is
// what availability must use.
//
// The bucket's own seats_limit is a snapshot of the limit that applied when it
// was last incremented, and two situations make it disagree with the venue:
//
//   - a deliberate overbooking (migration 0056) raises the bucket's limit to
//     exactly the seats it needed. Right after it, taken >= limit, so the answer
//     is 0 either way — but once that booking is CANCELLED the raised limit
//     lingers on the row (the release path deliberately never lowers a limit,
//     see 0054), and reading it alone would advertise seats the venue does not
//     have;
//   - a venue that lowered its declared capacity has buckets still stamped with
//     the old, larger number.
//
// In both the truthful answer is bounded by what the venue says it can seat
// today. The result is never negative: "how many more guests fit" is a quantity
// that can still be sold, and an oversold room sells nothing — the size of the
// overshoot is answered by the audit trail, not by an availability calendar.
func (u CapacityUsage) FreeUnder(declaredSeats int) int {
	limit := u.SeatsLimit
	if declaredSeats < limit {
		limit = declaredSeats
	}
	if u.SeatsTaken >= limit {
		return 0
	}
	return limit - u.SeatsTaken
}

// BookingCapacityOverride is one deliberate overbooking: staff seated a party
// the venue's declared capacity did not fit ("we'll bring a chair"). It is
// written once, in the same transaction as the booking and its holds, and never
// updated or deleted — migration 0056 enforces that in the database.
//
// SeatsOver / PeakBucketStart / PeakSeatsTaken describe the WORST of the
// booking's buckets: a visit spans about ten of them and each may be over by a
// different amount, so a single number would have to be either a lie or a list.
type BookingCapacityOverride struct {
	ID              uuid.UUID
	BookingID       uuid.UUID
	RestaurantID    uuid.UUID
	ActorUserID     uuid.UUID
	ActorType       ActorType
	Guests          int
	DeclaredSeats   int
	SeatsOver       int
	PeakBucketStart time.Time
	PeakSeatsTaken  int
	CreatedAt       time.Time
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
	// LockVenue takes a per-venue lock for the rest of the transaction. Both a
	// booking create and a capacity-policy change must take it before deriving
	// anything from the venue's policy: they each read the policy and then
	// write rows computed from it, so without a shared lock a create can
	// re-stamp buckets a policy change just rewrote, or commit an unheld
	// booking into a venue that became seats-mode mid-flight.
	LockVenue(ctx context.Context, restaurantID uuid.UUID) error
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

	// RecordOverride appends the audit row of one deliberate overbooking. Call
	// inside the same transaction as the booking: an override that is not
	// recorded must not exist. Returns ErrAlreadyExists if the booking already
	// has one — the row is unique per booking, which is what makes the write
	// exactly-once under a retry.
	RecordOverride(ctx context.Context, o BookingCapacityOverride) error
	// ListOverrides returns a venue's overbookings whose overloaded bucket lies
	// in [from, to), newest first. This is the "who put 12 people in a 10-seat
	// room last Saturday" query.
	ListOverrides(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]BookingCapacityOverride, error)
}
