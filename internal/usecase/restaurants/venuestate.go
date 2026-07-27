package restaurants

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// venueStateReader is the minimal batch read the catalog needs to tell a guest
// the truth about a venue's hours and bookability. Both methods take the whole
// page of restaurant ids so a 20-item listing costs two queries, not forty.
//
// A restaurant with no rows is simply ABSENT from the returned map — "no data"
// must stay distinguishable from "zero", because the two mean different things
// to the guest ("hours unknown" vs "closed" / "cannot book").
type venueStateReader interface {
	WorkingHoursFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error)
	// BookableTableCountsFor counts the tables that could actually seat a
	// party (is_active AND capacity > 0) — the same filter the availability
	// engine applies in usecase/bookings.loadSchedule.
	BookableTableCountsFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]int, error)
	// ScheduleOverridesFor reads the venues' special-day exceptions in the
	// window [from, to] (inclusive calendar dates). The window is widened by a
	// day on each side by the caller so it covers every venue's local dates
	// whatever timezone it sits in; the exact per-venue resolution happens in
	// the domain, through the SAME helper the availability engine uses.
	ScheduleOverridesFor(ctx context.Context, restaurantIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error)
}

// scheduleExceptionDays is how far ahead the public payload reports a venue's
// special days. Long enough for a guest planning the next few weeks (New Year,
// a holiday closure) and short enough that the list stays a handful of rows on
// a catalog page of twenty venues. The covered window is stated in the payload
// itself (WeeklySchedule.ExceptionsFrom/Until) so a client never has to guess
// whether an empty list means "no special days" or "we didn't look".
const scheduleExceptionDays = 30

// dateLayout is the calendar-date format the public payload speaks, identical
// to usecase/bookings.DateLayout (the availability endpoint's ?date=). Spelled
// out here rather than imported so this package keeps its one-way dependency
// on the booking layer limited to the small resolver interfaces above.
const dateLayout = "2006-01-02"

// VenueTimezoneResolver answers which IANA timezone a venue's own clock runs
// in. It is bound in bootstrap/deps.go to the SAME resolution the booking and
// availability engine uses (per-venue restaurants.timezone override, falling
// back to the platform-wide BOOKING_TIMEZONE_FALLBACK), so "open now" and
// "bookable slots" can never be computed against two different clocks.
type VenueTimezoneResolver interface {
	VenueTimezone(r domain.Restaurant) string
	// VenueCapacity reports which availability engine this venue runs on and,
	// in seats mode, how many guests it seats at once. Bound to the same
	// resolution the booking engine uses, so the catalog cannot disagree with
	// it about whether a venue is bookable.
	VenueCapacity(r domain.Restaurant) (domain.CapacityMode, int)
}

// VenueState computes the guest-facing venue state — structured weekly
// schedule, "open now", and "can this venue take an online booking at all" —
// and attaches it to catalog rows.
//
// It is a standalone unit rather than a method set on the catalog facade
// because MORE THAN ONE endpoint serves domain.RestaurantListItem rows: the
// catalog listing, the search, and the user's favorites all render the same
// public shape through transport/rest/restaurants.PublicListItem. Every one of
// them must attach the state, or the same venue reads differently on two
// screens — the guest would see "hours unknown" in favorites and a full
// schedule in the catalog, with no data reason for the difference.
type VenueState struct {
	reader venueStateReader
	tz     VenueTimezoneResolver
	clock  func() time.Time
}

// VenueStateOption configures optional VenueState dependencies.
type VenueStateOption func(*VenueState)

// WithVenueStateClock overrides the clock "open now" is evaluated against.
// Tests only.
func WithVenueStateClock(now func() time.Time) VenueStateOption {
	return func(v *VenueState) { v.clock = now }
}

// NewVenueState constructs the enricher. Both dependencies are required; a nil
// one makes every Attach* a no-op, so the fields are simply absent from the
// payload rather than guessed.
func NewVenueState(reader venueStateReader, tz VenueTimezoneResolver, opts ...VenueStateOption) *VenueState {
	v := &VenueState{reader: reader, tz: tz}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// buildVenueState computes the public schedule + open-now + bookability for one
// venue. It never invents data:
//   - no usable working-hours rows  → Schedule stays nil ("unknown"), and the
//     free-text opening_hours column is deliberately NOT parsed here;
//   - unknown timezone on this host → OpenNow stays nil ("unknown"), the day
//     list is still served (it is plain venue-local text, always true).
//   - overrides could not be read → OpenNow stays nil and the exceptions
//     window is absent: with unknown special days "open now" would be a guess,
//     and a guess is exactly what a holiday closure turns into a lie.
func buildVenueState(
	hours []domain.WorkingHours, overrides []domain.ScheduleOverride, overridesKnown bool,
	tz string,
	mode domain.CapacityMode, capacitySeats, bookableTables int,
	now time.Time,
) domain.PublicVenueState {
	days := domain.BuildWeeklySchedule(hours)

	var st domain.PublicVenueState
	openDays := 0
	for _, d := range days {
		if d.IsOpen {
			openDays++
		}
	}
	// "Has capacity" is MODE-DEPENDENT, and getting this wrong lies in the
	// direction that hurts most. A seats-mode venue (migration 0054) declares a
	// total number of guests instead of a table list, and its availability comes
	// from restaurant_capacity_buckets — the table list is not consulted at all.
	// Judging it by its tables would report accepts_online_bookings=false for
	// exactly the table-less venues seats mode exists to make bookable, and the
	// app hides the booking button on that flag.
	hasCapacity := bookableTables > 0
	if mode == domain.CapacityModeSeats {
		hasCapacity = capacitySeats > 0
	}
	// Without a readable open day the availability engine generates no start
	// times on ANY date, so the venue cannot be booked online however much
	// capacity it has — see PublicVenueState.AcceptsOnlineBookings.
	st.AcceptsOnlineBookings = hasCapacity && openDays > 0

	if len(days) == 0 {
		return st
	}
	sch := &domain.WeeklySchedule{Timezone: tz, Days: days}
	if loc, err := time.LoadLocation(tz); err == nil && overridesKnown {
		open := domain.IsOpenAt(hours, overrides, now, loc)
		sch.OpenNow = &open

		// The exceptions window is stated in the VENUE's local dates, from its
		// today: a date already past says nothing about the venue's next
		// month, and a client applying a stale closure would shut a venue that
		// is open.
		from := domain.StartOfDay(now, loc)
		sch.Exceptions = domain.BuildScheduleExceptions(overrides, from, scheduleExceptionDays, loc)
		sch.ExceptionsFrom = from.Format(dateLayout)
		sch.ExceptionsUntil = from.AddDate(0, 0, scheduleExceptionDays-1).Format(dateLayout)
	}
	st.Schedule = sch
	return st
}

// AttachList fills VenueState on every listing row, in place. It is
// best-effort by design: the catalog is the app's home screen, so a failure to
// read hours degrades to "unknown" (nil VenueState, fields omitted) instead of
// failing the whole response.
func (v *VenueState) AttachList(ctx context.Context, items []domain.RestaurantListItem) {
	if !v.usable() || len(items) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.Restaurant.ID)
	}
	now := v.now()
	data, ok := v.read(ctx, ids, now)
	if !ok {
		return
	}
	for i := range items {
		id := items[i].Restaurant.ID
		mode, seats := v.tz.VenueCapacity(items[i].Restaurant)
		st := buildVenueState(data.hours[id], data.overrides[id], data.overridesKnown,
			v.tz.VenueTimezone(items[i].Restaurant),
			mode, seats, data.tables[id], now)
		items[i].VenueState = &st
	}
}

// AttachOne is AttachList for the detail endpoint.
func (v *VenueState) AttachOne(ctx context.Context, agg *domain.RestaurantAggregate) {
	if !v.usable() || agg == nil {
		return
	}
	id := agg.Restaurant.ID
	now := v.now()
	data, ok := v.read(ctx, []uuid.UUID{id}, now)
	if !ok {
		return
	}
	mode, seats := v.tz.VenueCapacity(agg.Restaurant)
	st := buildVenueState(data.hours[id], data.overrides[id], data.overridesKnown,
		v.tz.VenueTimezone(agg.Restaurant),
		mode, seats, data.tables[id], now)
	agg.VenueState = &st
}

// usable reports whether this enricher can do anything at all. A nil receiver
// is valid and means "not wired", so a caller can hold a plain *VenueState
// without nil-checking at every use site.
func (v *VenueState) usable() bool { return v != nil && v.reader != nil && v.tz != nil }

// venueStateData is one batch read of everything the enrichment needs.
// overridesKnown is false when the special-day read failed — the catalog is
// still served, but without "open now" and without an exceptions window, so
// nothing in the payload silently ignores a closure it could not see.
type venueStateData struct {
	hours          map[uuid.UUID][]domain.WorkingHours
	tables         map[uuid.UUID]int
	overrides      map[uuid.UUID][]domain.ScheduleOverride
	overridesKnown bool
}

func (v *VenueState) read(ctx context.Context, ids []uuid.UUID, now time.Time) (venueStateData, bool) {
	hours, err := v.reader.WorkingHoursFor(ctx, ids)
	if err != nil {
		slog.Warn("venue working hours lookup failed, serving catalog without schedule", "error", err)
		return venueStateData{}, false
	}
	tables, err := v.reader.BookableTableCountsFor(ctx, ids)
	if err != nil {
		slog.Warn("venue table lookup failed, serving catalog without schedule", "error", err)
		return venueStateData{}, false
	}
	data := venueStateData{hours: hours, tables: tables}

	// The window is widened by a day on each side of the venue-local range: the
	// page may hold venues in different zones, and "today" in Almaty can still
	// be yesterday in Istanbul. The exact per-venue dates are then resolved in
	// the venue's own zone by domain.BuildScheduleExceptions, so the slack here
	// costs at most two extra rows per venue and can never cut a date short.
	// Yesterday is included for a second reason: "open now" at 00:30 has to see
	// the shift that started the previous evening.
	from := now.AddDate(0, 0, -1)
	to := now.AddDate(0, 0, scheduleExceptionDays)
	overrides, err := v.reader.ScheduleOverridesFor(ctx, ids, from, to)
	if err != nil {
		slog.Warn("venue schedule overrides lookup failed, serving catalog without open_now/exceptions", "error", err)
		return data, true
	}
	data.overrides, data.overridesKnown = overrides, true
	return data, true
}

// now is the enricher's clock, injectable so schedule tests can freeze an
// instant.
func (v *VenueState) now() time.Time {
	if v.clock != nil {
		return v.clock()
	}
	return time.Now()
}
