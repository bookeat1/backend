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
	u := NewAvailabilityUseCase(&fakeLinks{},
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		&fakeSchedule{}, testConfig())

	if _, err := u.Day(context.Background(), rid, "2026-08-03", 0); err == nil {
		t.Fatal("guests=0 must be rejected")
	}
	if _, err := u.Day(context.Background(), rid, "03.08.2026", 2); err == nil {
		t.Fatal("bad date must be rejected")
	}
}
