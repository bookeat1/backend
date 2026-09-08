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
// A bucket's own seats_limit is capped by the venue's current declared capacity
// (CapacityUsage.FreeUnder): a bucket may carry a LARGER limit than the venue —
// after a deliberate overbooking, or after the venue shrank — and neither means
// there are seats to sell.
func freeSeats(usage map[time.Time]domain.CapacityUsage, from, to time.Time, seatsLimit int) int {
	free := seatsLimit
	for _, bucket := range capacityBuckets(from, to) {
		u, ok := usage[bucket]
		if !ok {
			continue
		}
		if f := u.FreeUnder(seatsLimit); f < free {
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

// planCapacityOverride turns an ordinary set of holds into an OVERRIDING set:
// staff have decided to seat this party even though the declared capacity does
// not fit it (owner decision, 25.07.2026 — "we'll bring a chair").
//
// THE MECHANISM, and why it is this one. The holds are not exempted from
// anything; the seats they take are counted in restaurant_capacity_buckets like
// everybody else's, and chk_capacity_bucket_within_limit still decides. What
// changes is the limit each OVERFLOWING bucket is stamped with: exactly
// `already taken + this party`, i.e. precisely enough for this booking and not
// one seat more. Three properties follow, and all three are the point:
//
//   - the overbooking is VISIBLE in the ledger. The next booking is not sold the
//     same seats, because the bucket now reports zero room;
//   - the raise cannot be inherited. apply_capacity_delta re-stamps seats_limit
//     from the hold doing the increment, and an ordinary booking's holds carry
//     the plain declared capacity — so the very next non-overridden booking
//     trips the CHECK, whether or not the override was cancelled meanwhile;
//   - buckets the party fits into normally are left at the declared capacity, so
//     an "override" that turns out not to be needed changes nothing at all.
//
// Correctness rests on the caller holding the venue lock (LockVenue) before
// reading `usage`: this computes a limit from a value it has just read, which is
// exactly the read-then-write the rest of this package refuses to do without the
// lock. Every writer of these buckets takes the same lock.
//
// Returns the audit record, or nil when the booking fit after all — a party that
// fits is not an overbooking, and an audit trail that also records non-events is
// one nobody trusts. `holds` is modified in place.
func planCapacityOverride(
	b *domain.Booking,
	holds []domain.BookingCapacityHold,
	usage map[time.Time]domain.CapacityUsage,
	policy domain.BookingPolicy,
	acc access,
	actor Actor,
	now time.Time,
) *domain.BookingCapacityOverride {
	var worst *domain.BookingCapacityOverride
	for i := range holds {
		taken := 0
		if u, ok := usage[holds[i].BucketStart.UTC()]; ok {
			taken = u.SeatsTaken
		}
		total := taken + holds[i].Seats
		if total <= policy.CapacitySeats {
			continue
		}
		holds[i].SeatsLimit = total
		over := total - policy.CapacitySeats
		if worst != nil && over <= worst.SeatsOver {
			continue
		}
		worst = &domain.BookingCapacityOverride{
			ID: uuid.New(), BookingID: b.ID, RestaurantID: b.RestaurantID,
			ActorUserID: actor.UserID, ActorType: acc.actorType(),
			Guests: b.Guests, DeclaredSeats: policy.CapacitySeats,
			SeatsOver: over, PeakBucketStart: holds[i].BucketStart.UTC(),
			PeakSeatsTaken: total, CreatedAt: now,
		}
	}
	return worst
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
