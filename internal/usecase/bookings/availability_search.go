package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// AvailabilitySearch answers one question for MANY venues at once: "could this
// place seat N guests on this date, inside this time window?"
//
// WHY IT EXISTS. The catalog needed a filter by guests and date. The obvious
// implementation — a SQL predicate over tables and bookings — would have been a
// SECOND availability engine living next to the real one, and the two would
// have drifted the first time a venue got a special-day override or switched to
// seats mode. So this reuses the exact functions the per-venue slot grid runs
// on (candidateStarts, evaluateSlot, evaluateSeatsSlot): a venue survives the
// filter only if the booking screen would really offer it a slot.
//
// WHY IT IS NOT A LOOP OVER Day(). Day() reads five tables per venue. Over a
// catalog page that is a hundred round-trips, and over the whole catalog it
// grows without bound. Here every read is batched by venue id — a fixed number
// of queries whatever the catalog size — and only the pure evaluation runs per
// venue.
type AvailabilitySearch interface {
	// Filter returns the ids of the venues that have at least one bookable
	// start. A venue whose schedule cannot be read is simply NOT in the result:
	// "we could not tell" must never be published as "есть места".
	Filter(ctx context.Context, venues []domain.Restaurant, q domain.AvailabilitySearch) (map[uuid.UUID]bool, error)
}

// availabilityBatchReader is the per-venue day shape, read for many venues in
// one round-trip each. The first three methods already back the public catalog
// (usecase/restaurants.venueStateReader); the last two are what availability
// adds — the explicit start times and the tables themselves, where the catalog
// only ever needed a table COUNT.
type availabilityBatchReader interface {
	WorkingHoursFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error)
	ScheduleOverridesFor(ctx context.Context, restaurantIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error)
	TimeSlotsFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.TimeSlot, error)
	TablesFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.RestaurantTable, error)
}

// busyBatchReader is the occupancy of many venues in one query.
type busyBatchReader interface {
	ListBusyFor(ctx context.Context, restaurantIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.TableBusyInterval, error)
}

// capacityBatchReader is the same for seats-mode venues. It may be nil, in
// which case a seats-mode venue never survives the filter — the engine refuses
// to guess at occupancy it cannot see, exactly as Day() does.
type capacityBatchReader interface {
	ListUsageFor(ctx context.Context, restaurantIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.CapacityUsage, error)
}

type availabilitySearch struct {
	shape    availabilityBatchReader
	busy     busyBatchReader
	capacity capacityBatchReader
	cfg      Config
	now      func() time.Time
}

// NewAvailabilitySearch constructs the batch filter.
func NewAvailabilitySearch(
	shape availabilityBatchReader,
	busy busyBatchReader,
	capacity capacityBatchReader,
	cfg Config,
) AvailabilitySearch {
	return &availabilitySearch{
		shape: shape, busy: busy, capacity: capacity,
		cfg: cfg.withDefaults(), now: time.Now,
	}
}

// occupancySpanDays is how far around the requested date the occupancy read
// reaches. One day back and two forward, in UTC, because the date is a LOCAL
// date of each venue and venues sit up to 14 hours off UTC: a "Friday" in one
// zone can start before and end after the UTC Friday. Reading a little too much
// costs one extra index scan; reading too little would silently free a table
// that is in fact taken.
const occupancySpanDays = 1

func (s *availabilitySearch) Filter(
	ctx context.Context, venues []domain.Restaurant, q domain.AvailabilitySearch,
) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(venues))
	if len(venues) == 0 {
		return out, nil
	}
	if q.Guests <= 0 {
		return nil, fmt.Errorf("%w: guests must be positive", domain.ErrValidation)
	}
	// Parsed in UTC only to validate the shape and to bound the reads; each
	// venue re-parses it in its OWN location below.
	anchor, err := time.ParseInLocation(DateLayout, q.Date, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("%w: date must be YYYY-MM-DD", domain.ErrValidation)
	}
	if q.FromMinutes != nil && q.ToMinutes != nil && *q.ToMinutes < *q.FromMinutes {
		return nil, fmt.Errorf("%w: time window ends before it starts", domain.ErrValidation)
	}

	ids := make([]uuid.UUID, 0, len(venues))
	for _, v := range venues {
		ids = append(ids, v.ID)
	}
	from := anchor.AddDate(0, 0, -occupancySpanDays)
	to := anchor.AddDate(0, 0, occupancySpanDays+1)

	hours, err := s.shape.WorkingHoursFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	overrides, err := s.shape.ScheduleOverridesFor(ctx, ids,
		from.AddDate(0, 0, -overrideLookaround), to.AddDate(0, 0, overrideLookaround))
	if err != nil {
		return nil, err
	}
	slots, err := s.shape.TimeSlotsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	tables, err := s.shape.TablesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	busy, err := s.busy.ListBusyFor(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	usage := map[uuid.UUID][]domain.CapacityUsage{}
	if s.capacity != nil {
		usage, err = s.capacity.ListUsageFor(ctx, ids, from, to)
		if err != nil {
			return nil, err
		}
	}

	now := s.now()
	for _, v := range venues {
		if s.hasSlot(v, q, now, schedule{
			hours:     hours[v.ID],
			overrides: overrides[v.ID],
			slots:     slots[v.ID],
			tables:    activeTables(tables[v.ID]),
		}, busy[v.ID], usage[v.ID]) {
			out[v.ID] = true
		}
	}
	return out, nil
}

// hasSlot is the per-venue decision, and it is deliberately a SHORT-CIRCUIT:
// the catalog only needs to know whether a bookable start exists, not which
// ones, so a venue with a free 12:00 costs one evaluation instead of twenty.
func (s *availabilitySearch) hasSlot(
	r domain.Restaurant, q domain.AvailabilitySearch, now time.Time,
	sched schedule, busy []domain.TableBusyInterval, usage []domain.CapacityUsage,
) bool {
	policy := resolvePolicy(r, s.cfg)
	loc := policyLocation(policy)
	day, err := time.ParseInLocation(DateLayout, q.Date, loc)
	if err != nil {
		return false
	}
	starts := candidateStarts(sched, day, policy, s.cfg.SlotStep)
	if len(starts) == 0 {
		return false
	}

	if policy.CapacityMode == domain.CapacityModeSeats {
		// No capacity repository wired = no way to see occupancy. Day() errors
		// there; here, where one bad venue must not fail a whole catalog page,
		// the venue simply does not survive the filter.
		if s.capacity == nil {
			return false
		}
		idx := usageIndex(usage)
		for _, start := range starts {
			if !withinWindow(start, q) {
				continue
			}
			if evaluateSeatsSlot(start, q.Guests, policy, idx, now).Available {
				return true
			}
		}
		return false
	}

	for _, start := range starts {
		if !withinWindow(start, q) {
			continue
		}
		if evaluateSlot(start, q.Guests, policy, sched.tables, busy, now).Available {
			return true
		}
	}
	return false
}

// withinWindow reports whether a start lies inside the guest's requested time
// window, compared in the VENUE's local clock — the same clock the window was
// typed against on the screen.
func withinWindow(start time.Time, q domain.AvailabilitySearch) bool {
	if q.FromMinutes == nil && q.ToMinutes == nil {
		return true
	}
	mins := start.Hour()*60 + start.Minute()
	if q.FromMinutes != nil && mins < *q.FromMinutes {
		return false
	}
	if q.ToMinutes != nil && mins > *q.ToMinutes {
		return false
	}
	return true
}

// activeTables applies the same filter loadSchedule does — a table that is
// inactive or seats nobody is not a table the engine may sell.
func activeTables(tables []domain.RestaurantTable) []domain.RestaurantTable {
	out := make([]domain.RestaurantTable, 0, len(tables))
	for _, t := range tables {
		if t.IsActive && t.Capacity > 0 {
			out = append(out, t)
		}
	}
	return out
}
