package restaurants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeVenueState is a hand-written venueStateReader over two plain maps, close
// enough to the batch SQL: a restaurant with no rows is simply absent.
type fakeVenueState struct {
	hours        map[uuid.UUID][]domain.WorkingHours
	tables       map[uuid.UUID]int
	overrides    map[uuid.UUID][]domain.ScheduleOverride
	hoursErr     error
	tablesErr    error
	overridesErr error
	hoursIDs     [][]uuid.UUID  // every batch of ids the facade asked for
	overrideWins [][2]time.Time // the [from, to] window each overrides read asked for
}

func (f *fakeVenueState) WorkingHoursFor(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error) {
	f.hoursIDs = append(f.hoursIDs, ids)
	if f.hoursErr != nil {
		return nil, f.hoursErr
	}
	return f.hours, nil
}

func (f *fakeVenueState) BookableTableCountsFor(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]int, error) {
	if f.tablesErr != nil {
		return nil, f.tablesErr
	}
	return f.tables, nil
}

func (f *fakeVenueState) ScheduleOverridesFor(_ context.Context, _ []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error) {
	f.overrideWins = append(f.overrideWins, [2]time.Time{from, to})
	if f.overridesErr != nil {
		return nil, f.overridesErr
	}
	return f.overrides, nil
}

// fixedTZ resolves every venue to one zone, standing in for the booking
// engine's resolvePolicy (which deps.go binds for real).
type fixedTZ struct{ tz string }

func (f fixedTZ) VenueTimezone(domain.Restaurant) string { return f.tz }

// VenueCapacity stands in for the real resolver: it reports whatever the venue
// row itself declares, so a test can put a venue in seats mode by setting its
// override fields and nothing else.
func (f fixedTZ) VenueCapacity(r domain.Restaurant) (domain.CapacityMode, int) {
	return venueCapacityOf(r)
}

// perVenueTZ resolves each venue to its own stored override, exactly like the
// real resolver does, so a test can prove the flags follow the VENUE's clock.
type perVenueTZ struct{ fallback string }

func (p perVenueTZ) VenueTimezone(r domain.Restaurant) string {
	if tz := r.BookingPolicy.Timezone; tz != nil && *tz != "" {
		return *tz
	}
	return p.fallback
}

func (p perVenueTZ) VenueCapacity(r domain.Restaurant) (domain.CapacityMode, int) {
	return venueCapacityOf(r)
}

// venueCapacityOf mirrors usecase/bookings.resolvePolicy's reading of the two
// capacity columns: seats mode counts only when the venue declares a positive
// seat count, otherwise the venue is in table mode.
func venueCapacityOf(r domain.Restaurant) (domain.CapacityMode, int) {
	m, s := r.BookingPolicy.BookingCapacityMode, r.BookingPolicy.BookingCapacitySeats
	if m != nil && *m == domain.CapacityModeSeats && s != nil && *s > 0 {
		return domain.CapacityModeSeats, *s
	}
	return domain.CapacityModeTables, 0
}

func wh(dow int, isOpen bool, open, close_ string) domain.WorkingHours {
	w := domain.WorkingHours{DayOfWeek: dow, IsOpen: isOpen}
	if open != "" {
		w.OpenTime = &open
	}
	if close_ != "" {
		w.CloseTime = &close_
	}
	return w
}

// openEveryDay is the common "11:00 — 01:00, every day" shape of the live data.
func openEveryDay(open, close_ string) []domain.WorkingHours {
	out := make([]domain.WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		out = append(out, wh(dow, true, open, close_))
	}
	return out
}

func TestBuildVenueState(t *testing.T) {
	almaty := "Asia/Almaty"
	// 2026-07-25 00:30 Almaty is a Saturday, inside Friday's 11:00–01:00 tail.
	loc, err := time.LoadLocation(almaty)
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	afterMidnight := time.Date(2026, 7, 25, 0, 30, 0, 0, loc)
	middayFriday := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)

	tests := []struct {
		name  string
		hours []domain.WorkingHours
		tz    string
		// mode is the venue's resolved capacity mode. The zero value ("") is
		// neither constant, so it behaves like table mode — which is what a
		// venue that never opted in resolves to.
		mode        domain.CapacityMode
		seats       int
		tables      int
		now         time.Time
		wantSched   bool
		wantOpenNow *bool
		wantBooking bool
	}{
		{
			name:        "no working-hours rows: schedule unknown and nothing bookable",
			hours:       nil,
			tz:          almaty,
			tables:      4,
			now:         middayFriday,
			wantSched:   false,
			wantBooking: false,
		},
		{
			name:        "open across midnight, asked at 00:30, reads as open",
			hours:       openEveryDay("11:00", "01:00"),
			tz:          almaty,
			tables:      4,
			now:         afterMidnight,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: true,
		},
		{
			name:        "closed today reads as shut but stays bookable on other days",
			hours:       []domain.WorkingHours{wh(6, false, "", ""), wh(5, true, "11:00", "22:00")},
			tz:          almaty,
			tables:      4,
			now:         afterMidnight, // Saturday 00:30, Friday closed at 22:00
			wantSched:   true,
			wantOpenNow: boolp(false),
			wantBooking: true,
		},
		{
			name:        "hours but no tables: schedule known, bookings refused",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          almaty,
			tables:      0,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: false,
		},
		{
			name:        "tables but every day closed: nothing to book either",
			hours:       []domain.WorkingHours{wh(5, false, "", ""), wh(6, false, "", "")},
			tz:          almaty,
			tables:      9,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(false),
			wantBooking: false,
		},
		{
			name:        "unknown timezone: days still served, open_now stays unknown",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          "Mars/Olympus",
			tables:      4,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: nil,
			wantBooking: true,
		},
		// SEATS MODE (migration 0054). These are the venues the table-less
		// branch exists to unblock: they have NO tables on purpose, and judging
		// them by their table list is what made the flag lie.
		{
			name:        "seats mode with declared seats and no tables at all: BOOKABLE",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          almaty,
			mode:        domain.CapacityModeSeats,
			seats:       60,
			tables:      0,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: true,
		},
		{
			name:        "seats mode declaring zero seats: not bookable, tables cannot rescue it",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          almaty,
			mode:        domain.CapacityModeSeats,
			seats:       0,
			tables:      9,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: false,
		},
		{
			name:        "seats mode but every day closed: still nothing to book",
			hours:       []domain.WorkingHours{wh(5, false, "", ""), wh(6, false, "", "")},
			tz:          almaty,
			mode:        domain.CapacityModeSeats,
			seats:       60,
			tables:      0,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(false),
			wantBooking: false,
		},
		{
			name:        "table mode ignores a stale seat count",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          almaty,
			mode:        domain.CapacityModeTables,
			seats:       60, // left behind by a venue that switched back
			tables:      0,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := buildVenueState(tc.hours, nil, true, tc.tz, tc.mode, tc.seats, tc.tables, tc.now)
			if st.AcceptsOnlineBookings != tc.wantBooking {
				t.Errorf("AcceptsOnlineBookings = %v, want %v", st.AcceptsOnlineBookings, tc.wantBooking)
			}
			if tc.wantSched != (st.Schedule != nil) {
				t.Fatalf("Schedule present = %v, want %v", st.Schedule != nil, tc.wantSched)
			}
			if st.Schedule == nil {
				return
			}
			if st.Schedule.Timezone != tc.tz {
				t.Errorf("timezone = %q, want %q", st.Schedule.Timezone, tc.tz)
			}
			switch {
			case tc.wantOpenNow == nil && st.Schedule.OpenNow != nil:
				t.Errorf("OpenNow = %v, want unknown (nil)", *st.Schedule.OpenNow)
			case tc.wantOpenNow != nil && st.Schedule.OpenNow == nil:
				t.Errorf("OpenNow = nil, want %v", *tc.wantOpenNow)
			case tc.wantOpenNow != nil && *st.Schedule.OpenNow != *tc.wantOpenNow:
				t.Errorf("OpenNow = %v, want %v", *st.Schedule.OpenNow, *tc.wantOpenNow)
			}
		})
	}
}

// The same instant must produce different answers for venues in different
// zones — proof that "open now" follows the venue's clock, not the server's.
func TestListComputesOpenNowPerVenueTimezone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Istanbul"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	almatyVenue := uuid.New()
	istanbulVenue := uuid.New()
	istanbul := "Europe/Istanbul"

	repo := &fakeRestaurantRepo{list: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{ID: almatyVenue}},
		{Restaurant: domain.Restaurant{
			ID:            istanbulVenue,
			BookingPolicy: domain.BookingPolicyOverride{Timezone: &istanbul},
		}},
	}}
	state := &fakeVenueState{
		hours: map[uuid.UUID][]domain.WorkingHours{
			almatyVenue:   openEveryDay("11:00", "22:00"),
			istanbulVenue: openEveryDay("11:00", "22:00"),
		},
		tables: map[uuid.UUID]int{almatyVenue: 4, istanbulVenue: 4},
	}
	// 06:30 UTC = 11:30 in Almaty (open), 09:30 in Istanbul (still shut).
	instant := time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, perVenueTZ{fallback: "Asia/Almaty"},
			WithVenueStateClock(func() time.Time { return instant }))))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.VenueState == nil || it.VenueState.Schedule == nil || it.VenueState.Schedule.OpenNow == nil {
			t.Fatalf("venue %s: expected a computed open_now, got %+v", it.Restaurant.ID, it.VenueState)
		}
	}
	if !*items[0].VenueState.Schedule.OpenNow {
		t.Error("Almaty venue must be open at 11:30 local")
	}
	if *items[1].VenueState.Schedule.OpenNow {
		t.Error("Istanbul venue must be closed at 09:30 local")
	}
	if items[1].VenueState.Schedule.Timezone != istanbul {
		t.Errorf("timezone = %q, want %q", items[1].VenueState.Schedule.Timezone, istanbul)
	}
}

// Both the listing and the detail read must carry the fields; a venue with no
// tables ("Adept") must be flagged before the guest tries date after date.
func TestListAndGetAttachVenueState(t *testing.T) {
	id := uuid.New()
	rest := domain.Restaurant{ID: id}
	repo := &fakeRestaurantRepo{
		list: []domain.RestaurantListItem{{Restaurant: rest}},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	state := &fakeVenueState{
		hours:  map[uuid.UUID][]domain.WorkingHours{id: openEveryDay("11:00", "22:00")},
		tables: map[uuid.UUID]int{}, // no bookable tables at all
	}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, fixedTZ{tz: "Asia/Almaty"})))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if items[0].VenueState == nil || items[0].VenueState.Schedule == nil {
		t.Fatal("list item must carry the schedule")
	}
	if items[0].VenueState.AcceptsOnlineBookings {
		t.Error("a venue with no bookable tables must not claim to accept online bookings")
	}

	agg, err := f.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if agg.VenueState == nil || agg.VenueState.Schedule == nil {
		t.Fatal("detail must carry the schedule")
	}
	if agg.VenueState.AcceptsOnlineBookings {
		t.Error("detail must report the same bookability as the listing")
	}

	// One batch per read, not one query per restaurant.
	if len(state.hoursIDs) != 2 || len(state.hoursIDs[0]) != 1 || len(state.hoursIDs[1]) != 1 {
		t.Errorf("expected one batched lookup per read, got %v", state.hoursIDs)
	}
}

// The catalog is the app's home screen: a failed hours lookup must degrade to
// "unknown", never break the listing.
func TestVenueStateLookupFailureDegradesToUnknown(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{list: []domain.RestaurantListItem{{Restaurant: domain.Restaurant{ID: id}}}}
	state := &fakeVenueState{hoursErr: errors.New("db down")}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, fixedTZ{tz: "Asia/Almaty"})))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{})
	if err != nil {
		t.Fatalf("list must still succeed: %v", err)
	}
	if items[0].VenueState != nil {
		t.Error("a failed lookup must leave the venue state absent, not defaulted")
	}
}

// Without the option wired, nothing changes for existing callers.
func TestVenueStateAbsentWhenNotWired(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{
		list: []domain.RestaurantListItem{{Restaurant: domain.Restaurant{ID: id}}},
		agg:  &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}},
	}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if items[0].VenueState != nil {
		t.Error("venue state must be absent when the enrichment is not wired")
	}
	agg, err := f.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if agg.VenueState != nil {
		t.Error("venue state must be absent on detail when not wired")
	}
}

// TestListPublishesATableLessVenueAsBookable is the regression test for the
// interaction between the venue-state enricher and the table-less capacity mode
// (migration 0054, ADR-009).
//
// accepts_online_bookings was derived from "has at least one active table with
// capacity > 0". That was the whole truth while a table list was the only way to
// be bookable. It stopped being the truth the moment a venue could declare a
// seat count instead — and it fails in the WORST direction: the 17 live venues
// that keep no tables on purpose are exactly the ones seats mode exists to
// unblock, and they would still be published as unbookable. The app hides the
// booking button on this flag, so the feature would ship switched off for its
// only audience.
//
// The venue here has ZERO bookable tables, which is not an accident of the
// fixture — it is the point.
func TestListPublishesATableLessVenueAsBookable(t *testing.T) {
	if _, err := time.LoadLocation("Asia/Almaty"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	seatsVenue := uuid.New()
	tablesVenue := uuid.New()
	seatsMode := domain.CapacityModeSeats
	seats := 60

	repo := &fakeRestaurantRepo{list: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{
			ID: seatsVenue,
			BookingPolicy: domain.BookingPolicyOverride{
				BookingCapacityMode:  &seatsMode,
				BookingCapacitySeats: &seats,
			},
		}},
		// The control: same hours, same absence of tables, but no seats
		// declared. It must stay unbookable, so a passing test cannot be
		// explained by "the flag is now always true".
		{Restaurant: domain.Restaurant{ID: tablesVenue}},
	}}
	state := &fakeVenueState{
		hours: map[uuid.UUID][]domain.WorkingHours{
			seatsVenue:  openEveryDay("11:00", "22:00"),
			tablesVenue: openEveryDay("11:00", "22:00"),
		},
		tables: map[uuid.UUID]int{}, // neither venue has a single table
	}
	instant := time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, perVenueTZ{fallback: "Asia/Almaty"},
			WithVenueStateClock(func() time.Time { return instant }))))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{}, domain.VenueStateFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, it := range items {
		if it.VenueState == nil {
			t.Fatalf("venue %s: no venue state attached", it.Restaurant.ID)
		}
		got[it.Restaurant.ID] = it.VenueState.AcceptsOnlineBookings
	}
	if !got[seatsVenue] {
		t.Errorf("a seats-mode venue seating 60 guests with no tables is published as accepts_online_bookings=false — the flag still answers from the table list, so the venues table-less booking exists to unblock stay unbookable in the app")
	}
	if got[tablesVenue] {
		t.Errorf("a table-mode venue with no tables is published as bookable; the flag is not answering from capacity at all")
	}
}

// ---------------------------------------------------------------------------
// Special days in the PUBLIC payload.
//
// The catalog must tell the guest the truth for the dates it covers: a venue
// closed on an upcoming date has to be VISIBLE as closed, not merely missing
// from a weekly grid that keeps saying "open 11:00–22:00 every day".
// ---------------------------------------------------------------------------

func vsClosed(date string) domain.ScheduleOverride {
	return domain.ScheduleOverride{Date: vsDate(date), IsClosed: true}
}

func vsHours(date, open, close_ string) domain.ScheduleOverride {
	o, c := open, close_
	return domain.ScheduleOverride{Date: vsDate(date), OpenTime: &o, CloseTime: &c}
}

// vsDate mimics pgx's reading of a `date` column: midnight UTC.
func vsDate(date string) time.Time {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBuildVenueStateExceptions(t *testing.T) {
	tz := "Asia/Almaty"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// Friday 2026-07-24, 13:00 — the venue is inside its 11:00–22:00 window.
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)
	hours := openEveryDay("11:00", "22:00")

	tests := []struct {
		name           string
		overrides      []domain.ScheduleOverride
		overridesKnown bool
		wantOpenNow    *bool
		wantWindow     bool // exceptions_from/until published at all
		wantExceptions []domain.ScheduleException
	}{
		{
			name:           "no special days: the window is still stated, the list is empty",
			overridesKnown: true, wantOpenNow: boolp(true), wantWindow: true,
			wantExceptions: []domain.ScheduleException{},
		},
		{
			name:           "a closure TODAY flips open_now and is published as closed",
			overrides:      []domain.ScheduleOverride{vsClosed("2026-07-24")},
			overridesKnown: true, wantOpenNow: boolp(false), wantWindow: true,
			wantExceptions: []domain.ScheduleException{{Date: "2026-07-24"}},
		},
		{
			name:           "an upcoming closure is visible, and today stays open",
			overrides:      []domain.ScheduleOverride{vsClosed("2026-08-01")},
			overridesKnown: true, wantOpenNow: boolp(true), wantWindow: true,
			wantExceptions: []domain.ScheduleException{{Date: "2026-08-01"}},
		},
		{
			name:           "changed hours today are honoured by open_now and published",
			overrides:      []domain.ScheduleOverride{vsHours("2026-07-24", "18:00", "23:00")},
			overridesKnown: true, wantOpenNow: boolp(false), wantWindow: true,
			wantExceptions: []domain.ScheduleException{
				{Date: "2026-07-24", IsOpen: true, OpenTime: "18:00", CloseTime: "23:00"},
			},
		},
		{
			name:           "a past closure is neither applied nor published",
			overrides:      []domain.ScheduleOverride{vsClosed("2026-07-23")},
			overridesKnown: true, wantOpenNow: boolp(true), wantWindow: true,
			wantExceptions: []domain.ScheduleException{},
		},
		{
			name:           "a closure beyond the published horizon is not shown",
			overrides:      []domain.ScheduleOverride{vsClosed("2026-09-30")},
			overridesKnown: true, wantOpenNow: boolp(true), wantWindow: true,
			wantExceptions: []domain.ScheduleException{},
		},
		{
			name:      "overrides unreadable: no open_now and NO window — unknown, not 'none'",
			overrides: nil, overridesKnown: false,
			wantOpenNow: nil, wantWindow: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := buildVenueState(hours, tc.overrides, tc.overridesKnown, tz,
				domain.CapacityModeTables, 0, 4, now)
			if st.Schedule == nil {
				t.Fatal("schedule must be present: the venue has usable weekly hours")
			}
			switch {
			case tc.wantOpenNow == nil && st.Schedule.OpenNow != nil:
				t.Errorf("open_now = %v, want absent (unknown)", *st.Schedule.OpenNow)
			case tc.wantOpenNow != nil && st.Schedule.OpenNow == nil:
				t.Errorf("open_now absent, want %v", *tc.wantOpenNow)
			case tc.wantOpenNow != nil && *st.Schedule.OpenNow != *tc.wantOpenNow:
				t.Errorf("open_now = %v, want %v", *st.Schedule.OpenNow, *tc.wantOpenNow)
			}
			if !tc.wantWindow {
				if st.Schedule.ExceptionsFrom != "" || st.Schedule.ExceptionsUntil != "" {
					t.Errorf("window = %s..%s, want none: the server did not read the overrides",
						st.Schedule.ExceptionsFrom, st.Schedule.ExceptionsUntil)
				}
				if len(st.Schedule.Exceptions) != 0 {
					t.Errorf("exceptions = %+v, want none", st.Schedule.Exceptions)
				}
				return
			}
			if st.Schedule.ExceptionsFrom != "2026-07-24" || st.Schedule.ExceptionsUntil != "2026-08-22" {
				t.Errorf("window = %s..%s, want 2026-07-24..2026-08-22",
					st.Schedule.ExceptionsFrom, st.Schedule.ExceptionsUntil)
			}
			if len(st.Schedule.Exceptions) != len(tc.wantExceptions) {
				t.Fatalf("exceptions = %+v, want %+v", st.Schedule.Exceptions, tc.wantExceptions)
			}
			for i := range tc.wantExceptions {
				if st.Schedule.Exceptions[i] != tc.wantExceptions[i] {
					t.Errorf("exception[%d] = %+v, want %+v",
						i, st.Schedule.Exceptions[i], tc.wantExceptions[i])
				}
			}
		})
	}
}

// A venue shut for one holiday is not a venue that cannot be booked online:
// accepts_online_bookings is a static capability, and flipping it on a single
// date would hide the booking button for the other twenty-nine days.
func TestOverrideDoesNotChangeTheBookabilityFlag(t *testing.T) {
	tz := "Asia/Almaty"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)
	st := buildVenueState(openEveryDay("11:00", "22:00"),
		[]domain.ScheduleOverride{vsClosed("2026-07-24")}, true, tz,
		domain.CapacityModeTables, 0, 4, now)
	if !st.AcceptsOnlineBookings {
		t.Error("a one-day closure must not make the venue unbookable in general")
	}
}

// The batch read must cover yesterday (a shift that began the previous evening
// is still running at 00:30) and the whole published horizon.
func TestVenueStateAsksForTheWholeExceptionWindow(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)
	id := uuid.New()
	reader := &fakeVenueState{
		hours:  map[uuid.UUID][]domain.WorkingHours{id: openEveryDay("11:00", "22:00")},
		tables: map[uuid.UUID]int{id: 4},
	}
	v := NewVenueState(reader, fixedTZ{tz: "Asia/Almaty"},
		WithVenueStateClock(func() time.Time { return now }))
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}
	v.AttachOne(context.Background(), agg)

	if len(reader.overrideWins) != 1 {
		t.Fatalf("overrides read %d times, want exactly one batch", len(reader.overrideWins))
	}
	from, to := reader.overrideWins[0][0], reader.overrideWins[0][1]
	if !from.Before(now.AddDate(0, 0, -1).Add(time.Second)) {
		t.Errorf("window starts at %v, must reach back to yesterday for the past-midnight shift", from)
	}
	if to.Before(now.AddDate(0, 0, scheduleExceptionDays-1)) {
		t.Errorf("window ends at %v, must cover the whole published horizon", to)
	}
}

// A failed override read must not silently degrade into "the venue is open":
// the catalog is still served, but open_now and the window are absent.
func TestVenueStateDegradesWhenOverridesCannotBeRead(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)
	id := uuid.New()
	reader := &fakeVenueState{
		hours:        map[uuid.UUID][]domain.WorkingHours{id: openEveryDay("11:00", "22:00")},
		tables:       map[uuid.UUID]int{id: 4},
		overridesErr: errors.New("boom"),
	}
	v := NewVenueState(reader, fixedTZ{tz: "Asia/Almaty"},
		WithVenueStateClock(func() time.Time { return now }))
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}
	v.AttachOne(context.Background(), agg)

	if agg.VenueState == nil || agg.VenueState.Schedule == nil {
		t.Fatal("the weekly schedule must still be served")
	}
	if agg.VenueState.Schedule.OpenNow != nil {
		t.Errorf("open_now = %v, want absent: the special days were not readable",
			*agg.VenueState.Schedule.OpenNow)
	}
	if agg.VenueState.Schedule.ExceptionsFrom != "" {
		t.Error("an exceptions window must not be published when the read failed")
	}
}
