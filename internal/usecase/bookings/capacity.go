package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Table-less ("capacity") mode, migration 0054.
//
// A venue in this mode keeps no table list: it declares how many guests it can
// seat at once, availability answers "do N more guests still fit into this
// window?", and staff pick the actual table themselves — the way the legacy
// product worked, where a booking was a request rather than a seat assignment.
//
// Time is discretised into domain.CapacityBucket steps. A booking's occupancy
// window (the visit widened by the venue's buffer, i.e. exactly the interval
// table mode stores in booking_tables.slot) is expanded into the buckets it
// touches, and each bucket carries the full party size: guests occupy the room
// for the whole visit, not proportionally.

// maxCapacitySeats is the largest declared capacity the admin API accepts, and
// mirrors chk_restaurants_capacity_seats in migration 0054. See the migration
// for the reasoning: a real banquet hall in Almaty/Astana tops out around a
// thousand simultaneous guests, so 2000 rejects no honest venue while still
// catching the typo this field will actually see (an extra zero, or a phone
// number in the wrong box).
const maxCapacitySeats = 2000

// capacityBuckets expands the half-open window [from, to) into the bucket
// starts it touches.
//
// The lower end is floored to the grid, so a window starting at 19:07 claims
// the whole 19:00 bucket. That rounds OUTWARDS: the venue may end up holding up
// to one bucket more than the guests strictly need, but it can never hold less
// — the only direction in which an error is acceptable when the value being
// protected is "do not seat more people than fit".
func capacityBuckets(from, to time.Time) []time.Time {
	if !to.After(from) {
		return nil
	}
	start := from.UTC().Truncate(domain.CapacityBucket)
	out := make([]time.Time, 0, int(to.Sub(start)/domain.CapacityBucket)+1)
	for t := start; t.Before(to); t = t.Add(domain.CapacityBucket) {
		out = append(out, t)
	}
	return out
}

// buildCapacityHolds turns one booking into its per-bucket claims. seatsLimit
// is stamped onto every hold because the DB CHECK that enforces the limit lives
// on the aggregated bucket row and cannot read the venue (see 0054).
func buildCapacityHolds(b *domain.Booking, policy domain.BookingPolicy, now time.Time) []domain.BookingCapacityHold {
	from, to := occupancyWindow(b.StartsAt, policy)
	buckets := capacityBuckets(from, to)
	out := make([]domain.BookingCapacityHold, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, domain.BookingCapacityHold{
			ID: uuid.New(), BookingID: b.ID, RestaurantID: b.RestaurantID,
			BucketStart: bucket, Seats: b.Guests, SeatsLimit: policy.CapacitySeats,
			CreatedAt: now,
		})
	}
	return out
}

// freeSeats returns how many more guests fit in [from, to): the MINIMUM free
// seats across every bucket the window touches. The minimum, not the average or
// the endpoints — a party has to fit for the whole visit, and one full quarter
// of an hour in the middle is enough to make the slot unbookable.
//
// A bucket absent from `usage` has nothing sold in it and therefore offers the
// full declared capacity.
func freeSeats(usage map[time.Time]domain.CapacityUsage, from, to time.Time, seatsLimit int) int {
	free := seatsLimit
	for _, bucket := range capacityBuckets(from, to) {
		u, ok := usage[bucket]
		if !ok {
			continue
		}
		if f := u.Free(); f < free {
			free = f
		}
	}
	if free < 0 {
		return 0
	}
	return free
}

// usageIndex turns the repository's flat list into the lookup evaluateSeatsSlot
// needs. Keyed in UTC, matching capacityBuckets.
func usageIndex(rows []domain.CapacityUsage) map[time.Time]domain.CapacityUsage {
	out := make(map[time.Time]domain.CapacityUsage, len(rows))
	for _, r := range rows {
		out[r.BucketStart.UTC()] = r
	}
	return out
}

// evaluateSeatsSlot is the capacity-mode counterpart of evaluateSlot.
//
// The two reasons keep the meaning they have in table mode, translated:
//   - ReasonCapacity — the venue could not seat this party even when empty
//     (its whole declared capacity is smaller than the request);
//   - ReasonOccupied — it could, but the seats are already sold for this window.
//
// FreeTables is filled with the number of FURTHER PARTIES OF THIS SIZE that
// still fit. It is not an invented table count: it preserves exactly the two
// properties the existing client relies on — it is zero if and only if the slot
// is unavailable, and it grows with the room left. Clients that want the honest
// number read RemainingSeats.
func evaluateSeatsSlot(
	start time.Time,
	guests int,
	policy domain.BookingPolicy,
	usage map[time.Time]domain.CapacityUsage,
	now time.Time,
) Slot {
	s := Slot{StartsAt: start, EndsAt: start.Add(policy.Duration)}
	if reason := windowReason(start, policy, now); reason != "" {
		s.Reason = reason
		return s
	}
	if policy.CapacitySeats < guests {
		s.Reason = ReasonCapacity
		return s
	}
	from, to := occupancyWindow(start, policy)
	free := freeSeats(usage, from, to, policy.CapacitySeats)
	remaining := free
	s.RemainingSeats = &remaining
	s.FreeTables = free / guests
	if free < guests {
		s.Reason = ReasonOccupied
		return s
	}
	s.Available = true
	return s
}

// capacityReader is the minimal slice of the capacity repository the read-only
// consumers (availability, the mode switch) need. Declaring it here keeps the
// write side out of reach of anything that only computes a calendar.
type capacityReader interface {
	ListUsage(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.CapacityUsage, error)
	PeakTaken(ctx context.Context, restaurantID uuid.UUID, from time.Time) (*domain.CapacityUsage, error)
}

// venueLocker serialises the two writers that can disagree about a venue's
// capacity policy: a booking being created, and the policy itself being
// changed. Both read the policy and then write rows derived from it, so without
// a common lock the two interleave — a create that read "capacity 100" can
// re-stamp buckets a policy change just rewrote to 80, and a create that read
// "tables mode" can commit an unheld booking into a venue that has meanwhile
// become seats mode, invisible to the ledger. The lock is per venue and lives
// for the transaction, so venues never queue behind each other.
type venueLocker interface {
	LockVenue(ctx context.Context, restaurantID uuid.UUID) error
}

// checkCapacityGuests rejects a party the venue cannot seat at all before any
// availability data is read. The DB would refuse it anyway (the hold's seats
// exceed seats_limit, so the bucket CHECK trips), but a validation error is the
// truthful answer: "we are full right now" and "we are never that big" are
// different things to a guest.
func checkCapacityGuests(guests int, policy domain.BookingPolicy) error {
	if guests > policy.CapacitySeats {
		return fmt.Errorf("%w: the restaurant seats at most %d guests at a time",
			domain.ErrValidation, policy.CapacitySeats)
	}
	return nil
}
