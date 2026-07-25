package bookings

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// DateLayout is the calendar-day format accepted by the availability endpoint.
const DateLayout = "2006-01-02"

// Reasons a slot is not bookable. Returned to the client so the UI can explain
// itself instead of showing an unexplained greyed-out slot.
const (
	ReasonTooSoon = "too_soon"       // closer than policy.Lead
	ReasonHorizon = "beyond_horizon" // further than policy.HorizonDays
	// ReasonOccupied — the venue could seat the party, but not now: no free
	// table (or combination) left in table mode, no free seats in capacity mode.
	ReasonOccupied = "occupied"
	// ReasonCapacity — the venue could never seat this party: no table or
	// combination is big enough in table mode, the declared total capacity is
	// smaller than the request in capacity mode.
	ReasonCapacity = "capacity"
)

// Slot is one bookable start time of a day.
type Slot struct {
	StartsAt  time.Time
	EndsAt    time.Time
	Available bool
	// FreeTables is, in table mode, the tables free for the whole slot
	// (buffer included). In capacity mode the venue HAS no tables, so it
	// carries how many further parties OF THIS SIZE still fit — a number with
	// the same two properties the field always had (zero exactly when the slot
	// is unavailable, larger when there is more room) and without claiming a
	// table that does not exist. Read RemainingSeats for the honest figure.
	FreeTables int
	// RemainingSeats is the guests that still fit in this slot. Set only in
	// capacity mode; nil in table mode, where "seats left" is not a number the
	// venue's table layout can answer.
	RemainingSeats *int
	Reason         string // empty when Available
}

// DayAvailability is the answer of GET /api/restaurants/{id}/availability.
type DayAvailability struct {
	RestaurantID    uuid.UUID
	Date            string
	Timezone        string
	Guests          int
	DurationMinutes int
	// CapacityMode tells the client HOW to read the slots below: "tables" =
	// FreeTables is a table count, "seats" = the venue is table-less and
	// RemainingSeats is the meaningful figure.
	CapacityMode domain.CapacityMode
	// CapacitySeats is the venue's declared total capacity, non-zero only in
	// capacity mode. It lets the client render "5 of 40 seats left" without a
	// second request.
	CapacitySeats int
	Slots         []Slot
}

// AvailabilityUseCase computes bookable slots for one calendar day (spec §6).
// The endpoint is public — it takes no Actor.
type AvailabilityUseCase interface {
	Day(ctx context.Context, restaurantID uuid.UUID, date string, guests int) (*DayAvailability, error)
}

type availabilityUseCase struct {
	links       domain.BookingTableRepository
	capacity    capacityReader
	restaurants restaurantReader
	schedule    scheduleReader
	cfg         Config
}

// NewAvailabilityUseCase constructs the availability engine. capacity may be
// nil, in which case a venue configured for table-less mode simply reports no
// bookable slot — the engine never guesses at occupancy it cannot read.
func NewAvailabilityUseCase(
	links domain.BookingTableRepository,
	capacity capacityReader,
	restaurants restaurantReader,
	schedule scheduleReader,
	cfg Config,
) AvailabilityUseCase {
	return &availabilityUseCase{
		links: links, capacity: capacity, restaurants: restaurants,
		schedule: schedule, cfg: cfg.withDefaults(),
	}
}

func (u *availabilityUseCase) Day(ctx context.Context, restaurantID uuid.UUID, date string, guests int) (*DayAvailability, error) {
	if guests <= 0 {
		return nil, fmt.Errorf("%w: guests must be positive", domain.ErrValidation)
	}
	rest, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	policy := resolvePolicy(rest.Restaurant, u.cfg)
	loc := policyLocation(policy)
	day, err := time.ParseInLocation(DateLayout, date, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: date must be YYYY-MM-DD", domain.ErrValidation)
	}

	sched, err := u.loadSchedule(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	starts := candidateStarts(sched, day, policy, u.cfg.SlotStep)

	out := &DayAvailability{
		RestaurantID:    restaurantID,
		Date:            day.Format(DateLayout),
		Timezone:        policy.Timezone,
		Guests:          guests,
		DurationMinutes: int(policy.Duration / time.Minute),
		CapacityMode:    policy.CapacityMode,
		CapacitySeats:   policy.CapacitySeats,
		Slots:           make([]Slot, 0, len(starts)),
	}
	if len(starts) == 0 {
		return out, nil
	}

	// One occupancy query for the whole day, widened by the occupancy window so
	// a booking starting the previous evening still shows up.
	span := policy.Duration + 2*policy.Buffer
	from, to := starts[0].Add(-span), starts[len(starts)-1].Add(span)
	now := time.Now()

	if policy.CapacityMode == domain.CapacityModeSeats {
		usage, err := u.loadUsage(ctx, restaurantID, from, to)
		if err != nil {
			return nil, err
		}
		for _, start := range starts {
			out.Slots = append(out.Slots, evaluateSeatsSlot(start, guests, policy, usage, now))
		}
		return out, nil
	}

	busy, err := u.links.ListBusy(ctx, restaurantID, from, to)
	if err != nil {
		return nil, err
	}
	for _, start := range starts {
		out.Slots = append(out.Slots, evaluateSlot(start, guests, policy, sched.tables, busy, now))
	}
	return out, nil
}

// loadUsage reads the venue's sold seats for the day. With no capacity
// repository wired the engine refuses to answer rather than report an empty —
// i.e. wide open — day for a venue whose occupancy it cannot see.
func (u *availabilityUseCase) loadUsage(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) (map[time.Time]domain.CapacityUsage, error) {
	if u.capacity == nil {
		return nil, fmt.Errorf("%w: capacity availability is not configured", domain.ErrValidation)
	}
	rows, err := u.capacity.ListUsage(ctx, restaurantID, from, to)
	if err != nil {
		return nil, err
	}
	return usageIndex(rows), nil
}

// schedule is the venue's day-shape inputs, loaded once per request.
type schedule struct {
	hours  []domain.WorkingHours
	slots  []domain.TimeSlot
	tables []domain.RestaurantTable
}

func (u *availabilityUseCase) loadSchedule(ctx context.Context, restaurantID uuid.UUID) (schedule, error) {
	return loadSchedule(ctx, u.schedule, restaurantID)
}

// loadSchedule reads opening hours, bookable slots and the active tables of a
// venue. Inactive tables are dropped here so no caller can forget to.
func loadSchedule(ctx context.Context, r scheduleReader, restaurantID uuid.UUID) (schedule, error) {
	hours, err := r.ListWorkingHours(ctx, restaurantID)
	if err != nil {
		return schedule{}, err
	}
	slots, err := r.ListTimeSlots(ctx, restaurantID)
	if err != nil {
		return schedule{}, err
	}
	tables, err := r.ListTables(ctx, restaurantID)
	if err != nil {
		return schedule{}, err
	}
	active := make([]domain.RestaurantTable, 0, len(tables))
	for _, t := range tables {
		if t.IsActive && t.Capacity > 0 {
			active = append(active, t)
		}
	}
	return schedule{hours: hours, slots: slots, tables: active}, nil
}

// evaluateSlot decides whether one start time can seat the party.
func evaluateSlot(
	start time.Time,
	guests int,
	policy domain.BookingPolicy,
	tables []domain.RestaurantTable,
	busy []domain.TableBusyInterval,
	now time.Time,
) Slot {
	s := Slot{StartsAt: start, EndsAt: start.Add(policy.Duration)}
	if reason := windowReason(start, policy, now); reason != "" {
		s.Reason = reason
		return s
	}
	from, to := occupancyWindow(start, policy)
	free := freeTables(tables, busy, from, to)
	s.FreeTables = len(free)
	if picked := pickTables(free, guests); len(picked) > 0 {
		s.Available = true
		return s
	}
	if total := totalCapacity(tables); total < guests || len(pickTables(tables, guests)) == 0 {
		s.Reason = ReasonCapacity
		return s
	}
	s.Reason = ReasonOccupied
	return s
}

// windowReason returns the booking-window violation for start, or "" when the
// start lies inside [now+Lead, now+HorizonDays].
func windowReason(start time.Time, policy domain.BookingPolicy, now time.Time) string {
	if start.Before(now.Add(policy.Lead)) {
		return ReasonTooSoon
	}
	loc := policyLocation(policy)
	// Horizon is counted in calendar days of the venue, not in 24h chunks:
	// "60 days ahead" must mean the whole 60th day is still bookable.
	last := startOfDay(now.In(loc).AddDate(0, 0, policy.HorizonDays), loc).AddDate(0, 0, 1)
	if !start.Before(last) {
		return ReasonHorizon
	}
	return ""
}

// occupancyWindow is the interval during which the tables of a booking are
// unavailable: the visit widened by the venue's cleanup buffer on both sides.
// It matches the tstzrange stored in booking_tables.slot (spec §4.3).
func occupancyWindow(start time.Time, policy domain.BookingPolicy) (time.Time, time.Time) {
	return start.Add(-policy.Buffer), start.Add(policy.Duration).Add(policy.Buffer)
}

// freeTables returns the tables with no busy interval overlapping [from, to).
func freeTables(tables []domain.RestaurantTable, busy []domain.TableBusyInterval, from, to time.Time) []domain.RestaurantTable {
	blocked := make(map[uuid.UUID]struct{}, len(busy))
	for _, b := range busy {
		if b.From.Before(to) && from.Before(b.To) { // half-open overlap
			blocked[b.TableID] = struct{}{}
		}
	}
	out := make([]domain.RestaurantTable, 0, len(tables))
	for _, t := range tables {
		if _, bad := blocked[t.ID]; !bad {
			out = append(out, t)
		}
	}
	return out
}

// pickTables chooses the tables to seat guests: the smallest single table that
// fits, otherwise a greedy largest-first combination of at most
// maxCombinedTables (spec §6.3). Returns nil when the party cannot be seated.
func pickTables(tables []domain.RestaurantTable, guests int) []domain.RestaurantTable {
	if guests <= 0 || len(tables) == 0 {
		return nil
	}
	sorted := append([]domain.RestaurantTable(nil), tables...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Capacity != sorted[j].Capacity {
			return sorted[i].Capacity < sorted[j].Capacity
		}
		return sorted[i].ID.String() < sorted[j].ID.String() // deterministic
	})
	// Smallest single table that fits — least wasted capacity.
	for _, t := range sorted {
		if t.Capacity >= guests {
			return []domain.RestaurantTable{t}
		}
	}
	// Greedy combination, largest first, capped at maxCombinedTables.
	var picked []domain.RestaurantTable
	seats := 0
	for i := len(sorted) - 1; i >= 0 && len(picked) < maxCombinedTables; i-- {
		picked = append(picked, sorted[i])
		seats += sorted[i].Capacity
		if seats >= guests {
			return picked
		}
	}
	return nil
}

func totalCapacity(tables []domain.RestaurantTable) int {
	total := 0
	for _, t := range tables {
		total += t.Capacity
	}
	return total
}

// candidateStarts expands one calendar day into bookable start times in the
// venue's timezone.
//
// Semantics (deliberate, see the spec follow-ups): a restaurant_time_slots row
// is one bookable start time; rows flagged is_manually_disabled are skipped.
// When a venue has no slot rows for that weekday, starts are generated from the
// opening hours every cfg.SlotStep. In both cases the whole visit must fit
// inside the opening hours.
func candidateStarts(s schedule, day time.Time, policy domain.BookingPolicy, step time.Duration) []time.Time {
	loc := day.Location()
	dow := int(day.Weekday())
	open, close_, ok := openingWindow(s.hours, dow, day, loc)
	if !ok {
		return nil
	}

	var starts []time.Time
	explicit := false
	for _, ts := range s.slots {
		if ts.DayOfWeek != dow || ts.IsManuallyDisabled {
			continue
		}
		explicit = true
		mins, err := parseClock(ts.StartTime)
		if err != nil {
			continue
		}
		starts = append(starts, startOfDay(day, loc).Add(time.Duration(mins)*time.Minute))
	}
	if !explicit {
		for t := open; !t.Add(policy.Duration).After(close_); t = t.Add(step) {
			starts = append(starts, t)
		}
	}

	out := starts[:0]
	for _, t := range starts {
		if !t.Before(open) && !t.Add(policy.Duration).After(close_) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// openingWindow returns [open, close) for the weekday in the venue's timezone.
// A close time that is not after the open time is treated as past midnight
// (e.g. 18:00–02:00) and rolls into the next day.
func openingWindow(hours []domain.WorkingHours, dow int, day time.Time, loc *time.Location) (time.Time, time.Time, bool) {
	for _, h := range hours {
		if h.DayOfWeek != dow {
			continue
		}
		if !h.IsOpen || h.OpenTime == nil || h.CloseTime == nil {
			return time.Time{}, time.Time{}, false
		}
		openMin, err := parseClock(*h.OpenTime)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		closeMin, err := parseClock(*h.CloseTime)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		base := startOfDay(day, loc)
		open := base.Add(time.Duration(openMin) * time.Minute)
		close_ := base.Add(time.Duration(closeMin) * time.Minute)
		if !close_.After(open) {
			close_ = close_.AddDate(0, 0, 1)
		}
		return open, close_, true
	}
	return time.Time{}, time.Time{}, false
}

// startOfDay returns midnight of t's calendar day in loc. Built from the date
// parts (not by truncation) so DST transitions are handled by the location.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// parseClock parses "HH:MM" or "HH:MM:SS" into minutes since midnight.
func parseClock(v string) (int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("%w: bad clock value %q", domain.ErrValidation, v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("%w: bad clock value %q", domain.ErrValidation, v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("%w: bad clock value %q", domain.ErrValidation, v)
	}
	if h < 0 || h > 47 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: bad clock value %q", domain.ErrValidation, v)
	}
	return h*60 + m, nil
}
