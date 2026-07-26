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
	// BookableTableCountsFor counts the tables that could actually seat someone
	// (is_active AND capacity > 0) — the same filter the availability engine
	// applies in usecase/bookings.loadSchedule.
	BookableTableCountsFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]int, error)
}

// VenueTimezoneResolver answers which IANA timezone a venue's own clock runs
// in. It is bound in bootstrap/deps.go to the SAME resolution the booking and
// availability engine uses (per-venue restaurants.timezone override, falling
// back to the platform-wide BOOKING_TIMEZONE_FALLBACK), so "open now" and
// "bookable slots" can never be computed against two different clocks.
type VenueTimezoneResolver interface {
	VenueTimezone(r domain.Restaurant) string
}

// buildVenueState computes the public schedule + open-now + bookability for one
// venue. It never invents data:
//   - no usable working-hours rows  → Schedule stays nil ("unknown"), and the
//     free-text opening_hours column is deliberately NOT parsed here;
//   - unknown timezone on this host → OpenNow stays nil ("unknown"), the day
//     list is still served (it is plain venue-local text, always true).
func buildVenueState(hours []domain.WorkingHours, tz string, bookableTables int, now time.Time) domain.PublicVenueState {
	days := domain.BuildWeeklySchedule(hours)

	var st domain.PublicVenueState
	openDays := 0
	for _, d := range days {
		if d.IsOpen {
			openDays++
		}
	}
	// Without a readable open day the availability engine generates no start
	// times on ANY date, so the venue cannot be booked online however many
	// tables it has — see PublicVenueState.AcceptsOnlineBookings.
	st.AcceptsOnlineBookings = bookableTables > 0 && openDays > 0

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

// attachVenueState fills VenueState on every listing row. It is best-effort by
// design: the catalog is the app's home screen, so a failure to read hours
// degrades to "unknown" (nil VenueState, fields omitted) instead of failing the
// whole response.
func (f *facade) attachVenueState(ctx context.Context, items []domain.RestaurantListItem) {
	if f.venueState == nil || f.venueTZ == nil || len(items) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.Restaurant.ID)
	}
	hours, tables, ok := f.readVenueState(ctx, ids)
	if !ok {
		return
	}
	now := f.now()
	for i := range items {
		id := items[i].Restaurant.ID
		st := buildVenueState(hours[id], f.venueTZ.VenueTimezone(items[i].Restaurant), tables[id], now)
		items[i].VenueState = &st
	}
}

// attachVenueStateOne is attachVenueState for the detail endpoint.
func (f *facade) attachVenueStateOne(ctx context.Context, agg *domain.RestaurantAggregate) {
	if f.venueState == nil || f.venueTZ == nil || agg == nil {
		return
	}
	ids := []uuid.UUID{agg.Restaurant.ID}
	hours, tables, ok := f.readVenueState(ctx, ids)
	if !ok {
		return
	}
	st := buildVenueState(hours[agg.Restaurant.ID], f.venueTZ.VenueTimezone(agg.Restaurant),
		tables[agg.Restaurant.ID], f.now())
	agg.VenueState = &st
}

func (f *facade) readVenueState(ctx context.Context, ids []uuid.UUID) (
	map[uuid.UUID][]domain.WorkingHours, map[uuid.UUID]int, bool,
) {
	hours, err := f.venueState.WorkingHoursFor(ctx, ids)
	if err != nil {
		slog.Warn("venue working hours lookup failed, serving catalog without schedule", "error", err)
		return nil, nil, false
	}
	tables, err := f.venueState.BookableTableCountsFor(ctx, ids)
	if err != nil {
		slog.Warn("venue table lookup failed, serving catalog without schedule", "error", err)
		return nil, nil, false
	}
	return hours, tables, true
}

// now is the facade's clock, injectable so schedule tests can freeze an instant.
func (f *facade) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}
