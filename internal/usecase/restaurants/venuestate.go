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
}

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
func buildVenueState(
	hours []domain.WorkingHours, tz string,
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
	if loc, err := time.LoadLocation(tz); err == nil {
		open := domain.IsOpenAt(hours, now, loc)
		sch.OpenNow = &open
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
	hours, tables, ok := v.read(ctx, ids)
	if !ok {
		return
	}
	now := v.now()
	for i := range items {
		id := items[i].Restaurant.ID
		mode, seats := v.tz.VenueCapacity(items[i].Restaurant)
		st := buildVenueState(hours[id], v.tz.VenueTimezone(items[i].Restaurant),
			mode, seats, tables[id], now)
		items[i].VenueState = &st
	}
}

// AttachOne is AttachList for the detail endpoint.
func (v *VenueState) AttachOne(ctx context.Context, agg *domain.RestaurantAggregate) {
	if !v.usable() || agg == nil {
		return
	}
	id := agg.Restaurant.ID
	hours, tables, ok := v.read(ctx, []uuid.UUID{id})
	if !ok {
		return
	}
	mode, seats := v.tz.VenueCapacity(agg.Restaurant)
	st := buildVenueState(hours[id], v.tz.VenueTimezone(agg.Restaurant),
		mode, seats, tables[id], v.now())
	agg.VenueState = &st
}

// usable reports whether this enricher can do anything at all. A nil receiver
// is valid and means "not wired", so a caller can hold a plain *VenueState
// without nil-checking at every use site.
func (v *VenueState) usable() bool { return v != nil && v.reader != nil && v.tz != nil }

func (v *VenueState) read(ctx context.Context, ids []uuid.UUID) (
	map[uuid.UUID][]domain.WorkingHours, map[uuid.UUID]int, bool,
) {
	hours, err := v.reader.WorkingHoursFor(ctx, ids)
	if err != nil {
		slog.Warn("venue working hours lookup failed, serving catalog without schedule", "error", err)
		return nil, nil, false
	}
	tables, err := v.reader.BookableTableCountsFor(ctx, ids)
	if err != nil {
		slog.Warn("venue table lookup failed, serving catalog without schedule", "error", err)
		return nil, nil, false
	}
	return hours, tables, true
}

// now is the enricher's clock, injectable so schedule tests can freeze an
// instant.
func (v *VenueState) now() time.Time {
	if v.clock != nil {
		return v.clock()
	}
	return time.Now()
}
