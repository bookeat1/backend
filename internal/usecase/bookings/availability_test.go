package bookings

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func table(name string, capacity int) domain.RestaurantTable {
	return domain.RestaurantTable{ID: uuid.New(), Name: name, Capacity: capacity, IsActive: true}
}

func openAllWeek(open, close_ string) []domain.WorkingHours {
	out := make([]domain.WorkingHours, 0, 7)
	for d := 0; d < 7; d++ {
		o, c := open, close_
		out = append(out, domain.WorkingHours{DayOfWeek: d, OpenTime: &o, CloseTime: &c, IsOpen: true})
	}
	return out
}

func testPolicy(tz string) domain.BookingPolicy {
	return domain.BookingPolicy{
		Timezone: tz, Duration: 2 * time.Hour, Buffer: 15 * time.Minute,
		Lead: time.Hour, HorizonDays: 60, CancelDeadline: 3 * time.Hour,
		ConfirmSLA: 2 * time.Hour, MaxGuestsPerBooking: 20, AutoConfirm: true,
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable: %v", name, err)
	}
	return loc
}

func TestCandidateStartsFromWorkingHours(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, loc) // Monday
	s := schedule{hours: openAllWeek("12:00", "18:00")}

	starts := candidateStarts(s, day, testPolicy("Asia/Almaty"), 30*time.Minute)

	// 12:00 … 16:00 inclusive — the 2h visit must end by 18:00.
	if len(starts) != 9 {
		t.Fatalf("got %d starts, want 9: %v", len(starts), starts)
	}
	if !starts[0].Equal(time.Date(2026, 8, 3, 12, 0, 0, 0, loc)) {
		t.Fatalf("first start = %v", starts[0])
	}
	if !starts[len(starts)-1].Equal(time.Date(2026, 8, 3, 16, 0, 0, 0, loc)) {
		t.Fatalf("last start = %v", starts[len(starts)-1])
	}
}

func TestCandidateStartsClosedDay(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, loc)

	closed := openAllWeek("12:00", "18:00")
	closed[1].IsOpen = false
	if got := candidateStarts(schedule{hours: closed}, day, testPolicy("Asia/Almaty"), 30*time.Minute); len(got) != 0 {
		t.Fatalf("closed day returned %d starts", len(got))
	}
	// No working-hours row at all for the weekday → not bookable either.
	if got := candidateStarts(schedule{}, day, testPolicy("Asia/Almaty"), 30*time.Minute); len(got) != 0 {
		t.Fatalf("missing hours returned %d starts", len(got))
	}
}

func TestCandidateStartsExplicitSlots(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, loc) // Monday, dow=1
	s := schedule{
		hours: openAllWeek("12:00", "22:00"),
		slots: []domain.TimeSlot{
			{DayOfWeek: 1, StartTime: "13:00", EndTime: "15:00"},
			{DayOfWeek: 1, StartTime: "18:00", EndTime: "20:00"},
			{DayOfWeek: 1, StartTime: "21:00", EndTime: "23:00", IsManuallyDisabled: false}, // ends past closing
			{DayOfWeek: 1, StartTime: "16:00", EndTime: "18:00", IsManuallyDisabled: true},  // disabled
			{DayOfWeek: 2, StartTime: "14:00", EndTime: "16:00"},                            // other weekday
		},
	}

	starts := candidateStarts(s, day, testPolicy("Asia/Almaty"), 30*time.Minute)
	want := []time.Time{
		time.Date(2026, 8, 3, 13, 0, 0, 0, loc),
		time.Date(2026, 8, 3, 18, 0, 0, 0, loc),
	}
	if len(starts) != len(want) {
		t.Fatalf("got %v, want %v", starts, want)
	}
	for i := range want {
		if !starts[i].Equal(want[i]) {
			t.Fatalf("start[%d] = %v, want %v", i, starts[i], want[i])
		}
	}
}

// A venue closing after midnight must still expose late starts.
func TestCandidateStartsPastMidnight(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, loc)
	s := schedule{hours: openAllWeek("18:00", "02:00")}

	starts := candidateStarts(s, day, testPolicy("Asia/Almaty"), time.Hour)
	last := starts[len(starts)-1]
	if !last.Equal(time.Date(2026, 8, 4, 0, 0, 0, 0, loc)) {
		t.Fatalf("last start = %v, want 2026-08-04 00:00", last)
	}
}

// Day boundaries must be computed in the venue's location, including across a
// DST transition (Almaty has none; Berlin does).
func TestStartOfDayAcrossDST(t *testing.T) {
	loc := mustLoad(t, "Europe/Berlin")
	// 2026-03-29 is the European DST spring-forward day.
	noon := time.Date(2026, 3, 29, 12, 0, 0, 0, loc)
	start := startOfDay(noon, loc)
	if start.Hour() != 0 || start.Day() != 29 {
		t.Fatalf("startOfDay = %v", start)
	}
	if next := start.AddDate(0, 0, 1); next.Sub(start) != 23*time.Hour {
		t.Fatalf("spring-forward day length = %v, want 23h", next.Sub(start))
	}
}

func TestPickTables(t *testing.T) {
	t2, t4, t6 := table("t2", 2), table("t4", 4), table("t6", 6)
	all := []domain.RestaurantTable{t2, t4, t6}

	cases := []struct {
		name   string
		tables []domain.RestaurantTable
		guests int
		want   int // number of tables picked, 0 = cannot seat
	}{
		{"exact single table", all, 4, 1},
		{"smallest table that fits", all, 1, 1},
		{"combination of two", all, 9, 2},
		{"combination of three", all, 12, 3},
		{"beyond total capacity", all, 13, 0},
		{"no tables", nil, 2, 0},
		{"zero guests", all, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickTables(tc.tables, tc.guests)
			if len(got) != tc.want {
				t.Fatalf("pickTables(%d) = %d tables, want %d", tc.guests, len(got), tc.want)
			}
		})
	}

	// Least-waste rule: a party of 3 gets the 4-seater, not the 6-seater.
	if got := pickTables(all, 3); got[0].ID != t4.ID {
		t.Fatalf("party of 3 seated at %s, want t4", got[0].Name)
	}
	// More than maxCombinedTables would be needed → refuse.
	small := []domain.RestaurantTable{table("a", 2), table("b", 2), table("c", 2), table("d", 2)}
	if got := pickTables(small, 8); got != nil {
		t.Fatalf("expected refusal beyond %d tables, got %d", maxCombinedTables, len(got))
	}
}

func TestFreeTablesRespectsBuffer(t *testing.T) {
	loc := time.UTC
	t1 := table("t1", 4)
	policy := testPolicy("UTC") // buffer 15m, duration 2h

	// Existing booking 10:00–12:00 stored with its buffer: 09:45–12:15.
	busy := []domain.TableBusyInterval{{
		TableID: t1.ID,
		From:    time.Date(2026, 8, 3, 9, 45, 0, 0, loc),
		To:      time.Date(2026, 8, 3, 12, 15, 0, 0, loc),
	}}

	// A 12:00 start occupies 11:45–14:15 → overlaps the buffered slot.
	from, to := occupancyWindow(time.Date(2026, 8, 3, 12, 0, 0, 0, loc), policy)
	if free := freeTables([]domain.RestaurantTable{t1}, busy, from, to); len(free) != 0 {
		t.Fatalf("12:00 must be blocked by the 15m buffer, got %d free", len(free))
	}
	// A 12:30 start occupies 12:15–14:45 → touches the end, half-open, free.
	from, to = occupancyWindow(time.Date(2026, 8, 3, 12, 30, 0, 0, loc), policy)
	if free := freeTables([]domain.RestaurantTable{t1}, busy, from, to); len(free) != 1 {
		t.Fatalf("12:30 must be free, got %d free", len(free))
	}
}

func TestWindowReason(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, loc)
	policy := testPolicy("Asia/Almaty") // lead 1h, horizon 60 days

	cases := []struct {
		name  string
		start time.Time
		want  string
	}{
		{"inside the lead time", now.Add(30 * time.Minute), ReasonTooSoon},
		{"in the past", now.Add(-time.Hour), ReasonTooSoon},
		{"exactly at the lead boundary", now.Add(time.Hour), ""},
		{"comfortably ahead", now.AddDate(0, 0, 3), ""},
		{"last bookable day", now.AddDate(0, 0, 60), ""},
		{"beyond the horizon", now.AddDate(0, 0, 61), ReasonHorizon},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowReason(tc.start, policy, now); got != tc.want {
				t.Fatalf("windowReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvaluateSlot(t *testing.T) {
	loc := time.UTC
	policy := testPolicy("UTC")
	now := time.Date(2026, 8, 3, 8, 0, 0, 0, loc)
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, loc)
	t4 := table("t4", 4)
	tables := []domain.RestaurantTable{t4}

	if s := evaluateSlot(start, 4, policy, tables, nil, now); !s.Available || s.FreeTables != 1 {
		t.Fatalf("free slot = %+v", s)
	}
	busy := []domain.TableBusyInterval{{TableID: t4.ID, From: start, To: start.Add(time.Hour)}}
	if s := evaluateSlot(start, 4, policy, tables, busy, now); s.Available || s.Reason != ReasonOccupied {
		t.Fatalf("occupied slot = %+v", s)
	}
	if s := evaluateSlot(start, 9, policy, tables, nil, now); s.Available || s.Reason != ReasonCapacity {
		t.Fatalf("oversized party = %+v", s)
	}
	if s := evaluateSlot(now.Add(10*time.Minute), 2, policy, tables, nil, now); s.Available || s.Reason != ReasonTooSoon {
		t.Fatalf("too-soon slot = %+v", s)
	}
}

func TestAvailabilityDay(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()
	t4 := table("t4", 4)
	day := time.Now().In(loc).AddDate(0, 0, 7)
	date := day.Format(DateLayout)

	u := NewAvailabilityUseCase(
		&fakeLinks{busy: []domain.TableBusyInterval{{
			TableID: t4.ID,
			From:    time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc),
			To:      time.Date(day.Year(), day.Month(), day.Day(), 14, 0, 0, 0, loc),
		}}},
		newFakeCapacity(),
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		&fakeSchedule{hours: openAllWeek("12:00", "18:00"), tables: []domain.RestaurantTable{t4}},
		testConfig(),
	)

	got, err := u.Day(context.Background(), rid, date, 4)
	if err != nil {
		t.Fatalf("Day: %v", err)
	}
	if got.Timezone != "Asia/Almaty" || got.DurationMinutes != 120 {
		t.Fatalf("day meta = %+v", got)
	}
	if len(got.Slots) != 9 {
		t.Fatalf("got %d slots, want 9", len(got.Slots))
	}
	// 12:00 and 13:00 collide with the booking, 14:00 still does through the
	// 15-minute buffer, 14:30 is free again.
	byClock := map[string]Slot{}
	for _, s := range got.Slots {
		byClock[s.StartsAt.In(loc).Format("15:04")] = s
	}
	for _, blocked := range []string{"12:00", "13:00", "14:00"} {
		if s, ok := byClock[blocked]; !ok || s.Available {
			t.Fatalf("slot %s should be unavailable: %+v", blocked, s)
		}
	}
	if s, ok := byClock["14:30"]; !ok || !s.Available {
		t.Fatalf("slot 14:30 should be available: %+v", s)
	}
}

func TestAvailabilityDayValidation(t *testing.T) {
	rid := uuid.New()
	u := NewAvailabilityUseCase(&fakeLinks{}, newFakeCapacity(),
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		&fakeSchedule{}, testConfig())

	if _, err := u.Day(context.Background(), rid, "2026-08-03", 0); err == nil {
		t.Fatal("guests=0 must be rejected")
	}
	if _, err := u.Day(context.Background(), rid, "03.08.2026", 2); err == nil {
		t.Fatal("bad date must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Special-day overrides in the slot generator.
//
// The engine and the public catalog resolve a date through the SAME
// domain.OpeningWindow, so these cases mirror the domain table one level up:
// here they pin that the slot grid actually follows it.
// ---------------------------------------------------------------------------

func overrideClosed(date string) domain.ScheduleOverride {
	return domain.ScheduleOverride{Date: overrideDate(date), IsClosed: true}
}

func overrideHours(date, open, close_ string) domain.ScheduleOverride {
	o, c := open, close_
	return domain.ScheduleOverride{Date: overrideDate(date), OpenTime: &o, CloseTime: &c}
}

// overrideDate mimics what pgx hands back for a `date` column: midnight UTC.
func overrideDate(date string) time.Time {
	t, err := time.Parse(DateLayout, date)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCandidateStartsAppliesScheduleOverrides(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	policy := testPolicy("Asia/Almaty") // 2h visit
	// 2026-08-03 is a Monday (dow=1), 2026-08-04 a Tuesday.
	monday := time.Date(2026, 8, 3, 0, 0, 0, 0, loc)

	weekdayOff := openAllWeek("12:00", "18:00")
	weekdayOff[1].IsOpen = false // Monday normally shut

	tests := []struct {
		name      string
		sched     schedule
		day       time.Time
		wantFirst string // "HH:MM" venue-local, "" when no start at all
		wantLast  string
		wantCount int
	}{
		{
			name:      "a closed override sells nothing on that date",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideClosed("2026-08-03")}},
			day:       monday,
			wantCount: 0,
		},
		{
			name:      "a closed override for ANOTHER date leaves this one alone",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideClosed("2026-08-04")}},
			day:       monday,
			wantFirst: "12:00", wantLast: "16:00", wantCount: 9,
		},
		{
			name:      "changed hours move the whole grid",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideHours("2026-08-03", "16:00", "22:00")}},
			day:       monday,
			wantFirst: "16:00", wantLast: "20:00", wantCount: 9,
		},
		{
			name:      "an override opens a weekday the venue is normally shut",
			sched:     schedule{hours: weekdayOff, overrides: []domain.ScheduleOverride{overrideHours("2026-08-03", "12:00", "16:00")}},
			day:       monday,
			wantFirst: "12:00", wantLast: "14:00", wantCount: 5,
		},
		{
			name:      "a special day may cross midnight",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideHours("2026-08-03", "20:00", "02:00")}},
			day:       monday,
			wantFirst: "20:00", wantLast: "00:00", wantCount: 9,
		},
		{
			name:      "a PAST override does not shut today",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideClosed("2025-01-01")}},
			day:       monday,
			wantFirst: "12:00", wantLast: "16:00", wantCount: 9,
		},
		{
			name: "duplicates: the first row wins, exactly as the storefront shows it",
			sched: schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{
				overrideHours("2026-08-03", "16:00", "22:00"), overrideClosed("2026-08-03"),
			}},
			day:       monday,
			wantFirst: "16:00", wantLast: "20:00", wantCount: 9,
		},
		{
			name: "explicit time-slot rows are cut down to the special hours",
			sched: schedule{
				hours: openAllWeek("12:00", "22:00"),
				slots: []domain.TimeSlot{
					{DayOfWeek: 1, StartTime: "13:00"}, {DayOfWeek: 1, StartTime: "18:00"},
				},
				overrides: []domain.ScheduleOverride{overrideHours("2026-08-03", "17:00", "21:00")},
			},
			day:       monday,
			wantFirst: "18:00", wantLast: "18:00", wantCount: 1,
		},
		{
			name: "explicit time-slot rows sell nothing on a closed date",
			sched: schedule{
				hours:     openAllWeek("12:00", "22:00"),
				slots:     []domain.TimeSlot{{DayOfWeek: 1, StartTime: "13:00"}},
				overrides: []domain.ScheduleOverride{overrideClosed("2026-08-03")},
			},
			day:       monday,
			wantCount: 0,
		},
		{
			name:      "an unreadable override sells nothing rather than falling back",
			sched:     schedule{hours: openAllWeek("12:00", "18:00"), overrides: []domain.ScheduleOverride{overrideHours("2026-08-03", "soon", "later")}},
			day:       monday,
			wantCount: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			starts := candidateStarts(tc.sched, tc.day, policy, 30*time.Minute)
			if len(starts) != tc.wantCount {
				t.Fatalf("got %d starts (%v), want %d", len(starts), starts, tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			if got := starts[0].In(loc).Format("15:04"); got != tc.wantFirst {
				t.Errorf("first start = %s, want %s", got, tc.wantFirst)
			}
			if got := starts[len(starts)-1].In(loc).Format("15:04"); got != tc.wantLast {
				t.Errorf("last start = %s, want %s", got, tc.wantLast)
			}
		})
	}
}

// The whole reason overrides live in `schedule`: a booking may not be created
// on a date the venue closed, and the check is the same window the availability
// grid is built from.
func TestWithinOpeningHoursHonoursOverrides(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	policy := testPolicy("Asia/Almaty")
	start := time.Date(2026, 8, 3, 13, 0, 0, 0, loc) // Monday 13:00, inside 12:00–18:00

	open := schedule{hours: openAllWeek("12:00", "18:00")}
	if !withinOpeningHours(open, start, policy) {
		t.Fatal("a normal Monday must accept a 13:00 booking")
	}
	closed := open
	closed.overrides = []domain.ScheduleOverride{overrideClosed("2026-08-03")}
	if withinOpeningHours(closed, start, policy) {
		t.Error("a date closed by an override must not accept a booking")
	}
	moved := open
	moved.overrides = []domain.ScheduleOverride{overrideHours("2026-08-03", "16:00", "22:00")}
	if withinOpeningHours(moved, start, policy) {
		t.Error("13:00 is outside the special 16:00–22:00 hours")
	}
	if !withinOpeningHours(moved, time.Date(2026, 8, 3, 17, 0, 0, 0, loc), policy) {
		t.Error("17:00 is inside the special hours and must be accepted")
	}
}

// The override read is BOUNDED, and the bound is anchored on the date the
// caller asked about — never on time.Now(). That distinction is the whole
// contract: availability for a date far in the past must resolve its special
// day exactly like a future one, which is why the query used to be unbounded.
//
// The fake honours the window, so a window computed from "now" would silently
// drop these overrides and the venue would look open.
func TestAvailabilityLoadsOverridesAroundTheRequestedDate(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()

	newUC := func(sch *fakeSchedule) AvailabilityUseCase {
		return NewAvailabilityUseCase(&fakeLinks{}, newFakeCapacity(),
			&fakeRestaurants{agg: &domain.RestaurantAggregate{
				Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
			sch, testConfig())
	}

	dates := []struct {
		name string
		date string
	}{
		{"a date years in the past", "2019-03-14"},
		{"a date years in the future", "2031-11-08"},
	}
	for _, tc := range dates {
		t.Run(tc.name+" still sees its closure", func(t *testing.T) {
			sch := &fakeSchedule{
				hours:     openAllWeek("12:00", "18:00"),
				overrides: []domain.ScheduleOverride{overrideClosed(tc.date)},
				tables:    []domain.RestaurantTable{table("t4", 4)},
			}
			got, err := newUC(sch).Day(context.Background(), rid, tc.date, 4)
			if err != nil {
				t.Fatalf("Day: %v", err)
			}
			if len(got.Slots) != 0 {
				t.Fatalf("closed override on %s produced %d slots — the engine did not load it",
					tc.date, len(got.Slots))
			}
			// The window must bracket the requested date, and tightly: it is a
			// couple of days of slack, not the venue's whole history.
			want, _ := time.ParseInLocation(DateLayout, tc.date, loc)
			if !sch.overrideFrom.Before(want) || !sch.overrideTo.After(want) {
				t.Errorf("override window [%s, %s] does not bracket %s",
					sch.overrideFrom.Format(DateLayout), sch.overrideTo.Format(DateLayout), tc.date)
			}
			if span := sch.overrideTo.Sub(sch.overrideFrom); span > 6*24*time.Hour {
				t.Errorf("override window spans %v — it is meant to be a few days around the date, not open-ended", span)
			}
		})
	}

	// An override outside the window must not be read at all: it can never
	// change the answer, and reading it is exactly the unbounded growth the
	// bound exists to stop.
	t.Run("an override on an unrelated date is not loaded", func(t *testing.T) {
		sch := &fakeSchedule{
			hours:     openAllWeek("12:00", "18:00"),
			overrides: []domain.ScheduleOverride{overrideClosed("2019-03-14"), overrideClosed("2031-11-08")},
			tables:    []domain.RestaurantTable{table("t4", 4)},
		}
		day := time.Now().In(loc).AddDate(0, 0, 7)
		got, err := newUC(sch).Day(context.Background(), rid, day.Format(DateLayout), 4)
		if err != nil {
			t.Fatalf("Day: %v", err)
		}
		if len(got.Slots) == 0 {
			t.Fatal("an ordinary day sold nothing: a distant override leaked into it")
		}
	})
}
