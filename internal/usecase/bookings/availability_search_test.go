package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The catalog filter's promise is narrow and worth stating: a venue survives
// "на двоих в пятницу" ONLY if the booking screen would really offer that
// party a start time. Everything below is a way the two could drift apart —
// occupancy, a holiday closure, a party too big for the room, a time window —
// and what a guest would experience if they did: a venue in the results that
// cannot seat them, which they find out only after choosing it.

type fakeShapeReader struct {
	hours     map[uuid.UUID][]domain.WorkingHours
	overrides map[uuid.UUID][]domain.ScheduleOverride
	slots     map[uuid.UUID][]domain.TimeSlot
	tables    map[uuid.UUID][]domain.RestaurantTable
	err       error
}

func (f *fakeShapeReader) WorkingHoursFor(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error) {
	return f.hours, f.err
}

func (f *fakeShapeReader) ScheduleOverridesFor(_ context.Context, _ []uuid.UUID, _, _ time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error) {
	return f.overrides, nil
}

func (f *fakeShapeReader) TimeSlotsFor(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]domain.TimeSlot, error) {
	return f.slots, nil
}

func (f *fakeShapeReader) TablesFor(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]domain.RestaurantTable, error) {
	return f.tables, nil
}

type fakeBusyReader struct {
	busy  map[uuid.UUID][]domain.TableBusyInterval
	calls int
}

func (f *fakeBusyReader) ListBusyFor(_ context.Context, _ []uuid.UUID, _, _ time.Time) (map[uuid.UUID][]domain.TableBusyInterval, error) {
	f.calls++
	return f.busy, nil
}

// searchFixture builds a venue open 12:00-22:00 Almaty time with the given
// tables, and a search engine whose clock sits well before the date under test
// so the booking window never interferes.
func searchFixture(t *testing.T, tables []domain.RestaurantTable, busy []domain.TableBusyInterval) (
	*availabilitySearch, domain.Restaurant, *fakeBusyReader,
) {
	t.Helper()
	mustLoad(t, "Asia/Almaty")
	id := uuid.New()
	tz := "Asia/Almaty"
	venue := domain.Restaurant{ID: id, BookingPolicy: domain.BookingPolicyOverride{Timezone: &tz}}

	shape := &fakeShapeReader{
		hours:  map[uuid.UUID][]domain.WorkingHours{id: openAllWeek("12:00", "22:00")},
		tables: map[uuid.UUID][]domain.RestaurantTable{id: tables},
	}
	busyReader := &fakeBusyReader{busy: map[uuid.UUID][]domain.TableBusyInterval{id: busy}}
	s := NewAvailabilitySearch(shape, busyReader, nil, Config{}).(*availabilitySearch)
	s.now = func() time.Time { return time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC) }
	return s, venue, busyReader
}

func TestAvailabilitySearchKeepsVenueWithAFreeTable(t *testing.T) {
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{table("T1", 2)}, nil)

	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !free[venue.ID] {
		t.Fatal("venue with a free two-seater must survive a search for two")
	}
}

func TestAvailabilitySearchDropsVenueWhoseTablesAreAllTaken(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	only := table("T1", 2)
	// One booking per candidate start would be tedious; the venue's whole
	// working day is covered instead, so no start can find the table free.
	busy := []domain.TableBusyInterval{{
		TableID: only.ID,
		From:    time.Date(2026, 8, 21, 10, 0, 0, 0, loc),
		To:      time.Date(2026, 8, 22, 2, 0, 0, 0, loc),
	}}
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{only}, busy)

	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if free[venue.ID] {
		t.Fatal("a fully booked venue must not appear under a search for a free table")
	}
}

func TestAvailabilitySearchDropsPartyTooBigForTheRoom(t *testing.T) {
	// Two two-seaters can be joined for four, never for nine: pickTables caps
	// a combination at three tables and there is not enough capacity anyway.
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{table("T1", 2), table("T2", 2)}, nil)

	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 9})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if free[venue.ID] {
		t.Fatal("a venue that could never seat nine must not appear in a search for nine")
	}
}

func TestAvailabilitySearchHonoursAHolidayClosure(t *testing.T) {
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{table("T1", 4)}, nil)
	// The venue is closed that day by a special-day override. Its weekly hours
	// still say "open 12:00-22:00" — this is exactly the case where a second,
	// SQL-side implementation of availability would have sold a table on a day
	// the venue had declared closed.
	loc := mustLoad(t, "Asia/Almaty")
	s.shape.(*fakeShapeReader).overrides = map[uuid.UUID][]domain.ScheduleOverride{
		venue.ID: {{
			RestaurantID: venue.ID,
			Date:         time.Date(2026, 8, 21, 0, 0, 0, 0, loc),
			IsClosed:     true,
		}},
	}

	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if free[venue.ID] {
		t.Fatal("a venue closed by a special-day override must not appear in the results")
	}
}

func TestAvailabilitySearchRespectsTheTimeWindow(t *testing.T) {
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{table("T1", 2)}, nil)
	// The venue closes at 22:00 and a visit lasts two hours, so the last start
	// is 20:00. A guest asking for 21:00 or later has to see nothing rather
	// than the 20:00 the venue does have.
	from := 21 * 60
	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2, FromMinutes: &from})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if free[venue.ID] {
		t.Fatal("a start outside the requested window must not keep the venue")
	}

	within := 19 * 60
	free, err = s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2, FromMinutes: &within})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !free[venue.ID] {
		t.Fatal("a venue with a free 19:00 must survive a search from 19:00")
	}
}

func TestAvailabilitySearchReadsOccupancyOncePerPage(t *testing.T) {
	// The whole reason this exists next to Day(): the cost of the filter must
	// not grow with the number of venues on the page.
	s, first, busyReader := searchFixture(t, []domain.RestaurantTable{table("T1", 2)}, nil)
	shape := s.shape.(*fakeShapeReader)
	venues := []domain.Restaurant{first}
	for i := 0; i < 9; i++ {
		id := uuid.New()
		tz := "Asia/Almaty"
		venues = append(venues, domain.Restaurant{ID: id, BookingPolicy: domain.BookingPolicyOverride{Timezone: &tz}})
		shape.hours[id] = openAllWeek("12:00", "22:00")
		shape.tables[id] = []domain.RestaurantTable{table("T1", 2)}
	}

	free, err := s.Filter(context.Background(), venues,
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(free) != len(venues) {
		t.Fatalf("kept %d of %d venues", len(free), len(venues))
	}
	if busyReader.calls != 1 {
		t.Fatalf("occupancy read %d times for one page, want 1", busyReader.calls)
	}
}

func TestAvailabilitySearchRejectsNonsenseQuery(t *testing.T) {
	s, venue, _ := searchFixture(t, []domain.RestaurantTable{table("T1", 2)}, nil)
	venues := []domain.Restaurant{venue}

	if _, err := s.Filter(context.Background(), venues,
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 0}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero guests: got %v, want a validation error", err)
	}
	if _, err := s.Filter(context.Background(), venues,
		domain.AvailabilitySearch{Date: "21.08.2026", Guests: 2}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad date: got %v, want a validation error", err)
	}
	from, to := 21*60, 19*60
	if _, err := s.Filter(context.Background(), venues, domain.AvailabilitySearch{
		Date: "2026-08-21", Guests: 2, FromMinutes: &from, ToMinutes: &to,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("inverted window: got %v, want a validation error", err)
	}
}

func TestAvailabilitySearchDropsSeatsModeVenueWithNoCapacityReader(t *testing.T) {
	// A seats-mode venue with no capacity repository wired is a venue whose
	// occupancy we cannot see. It must fall out of the filter rather than be
	// published as having room — the same refusal Day() makes, except one bad
	// venue here must not fail the whole catalog page.
	s, venue, _ := searchFixture(t, nil, nil)
	mode := domain.CapacityModeSeats
	seats := 40
	venue.BookingPolicy.BookingCapacityMode = &mode
	venue.BookingPolicy.BookingCapacitySeats = &seats

	free, err := s.Filter(context.Background(), []domain.Restaurant{venue},
		domain.AvailabilitySearch{Date: "2026-08-21", Guests: 2})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if free[venue.ID] {
		t.Fatal("a venue whose occupancy cannot be read must not survive the filter")
	}
}
