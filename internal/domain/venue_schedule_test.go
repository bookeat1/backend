package domain

import (
	"testing"
	"time"
)

func hoursp(s string) *string { return &s }

// day builds one restaurant_working_hours row. Empty times mean the columns are
// NULL (the shape the legacy import writes for a closed day).
func day(dow int, isOpen bool, open, close_ string) WorkingHours {
	w := WorkingHours{DayOfWeek: dow, IsOpen: isOpen}
	if open != "" {
		w.OpenTime = hoursp(open)
	}
	if close_ != "" {
		w.CloseTime = hoursp(close_)
	}
	return w
}

// almaty is the venue zone the live data actually uses; istanbul is a second,
// differently-offset zone so a test can prove "open now" follows the VENUE, not
// the process.
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable on this host: %v", name, err)
	}
	return loc
}

// A venue open 11:00–01:00 on Friday must read as OPEN at 00:30 on Saturday,
// even when Saturday itself is a day off — that is the case the client's
// first-and-last-time parsing gets wrong today.
func TestIsOpenAtCrossesMidnightAndClosedDays(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")

	// 2026-07-24 is a Friday.
	week := []WorkingHours{
		day(int(time.Sunday), true, "11:00", "22:00"),
		day(int(time.Monday), false, "", ""),
		day(int(time.Tuesday), true, "11:00", "22:00"),
		day(int(time.Wednesday), true, "11:00", "22:00"),
		day(int(time.Thursday), true, "11:00", "22:00"),
		day(int(time.Friday), true, "11:00", "01:00"),
		// Saturday deliberately absent: no row at all.
	}

	tests := []struct {
		name  string
		local time.Time
		want  bool
	}{
		{"friday midday inside window", time.Date(2026, 7, 24, 13, 0, 0, 0, almaty), true},
		{"friday just before opening", time.Date(2026, 7, 24, 10, 59, 0, 0, almaty), false},
		{"friday late evening still open", time.Date(2026, 7, 24, 23, 59, 0, 0, almaty), true},
		{"saturday 00:30 is friday's tail", time.Date(2026, 7, 25, 0, 30, 0, 0, almaty), true},
		{"saturday 01:00 is the exclusive end", time.Date(2026, 7, 25, 1, 0, 0, 0, almaty), false},
		{"saturday midday has no row at all", time.Date(2026, 7, 25, 13, 0, 0, 0, almaty), false},
		{"monday is explicitly closed", time.Date(2026, 7, 27, 13, 0, 0, 0, almaty), false},
		{"tuesday 22:00 is the exclusive end", time.Date(2026, 7, 28, 22, 0, 0, 0, almaty), false},
		{"tuesday 21:59 still open", time.Date(2026, 7, 28, 21, 59, 0, 0, almaty), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOpenAt(week, tc.local, almaty); got != tc.want {
				t.Errorf("IsOpenAt(%s) = %v, want %v", tc.local.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// The venue's own timezone decides, not the server's and not the guest's: the
// SAME instant is open for an Almaty venue and shut for an Istanbul one
// (Asia/Almaty is UTC+5, Europe/Istanbul UTC+3).
func TestIsOpenAtUsesTheVenueTimezone(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	istanbul := mustLoad(t, "Europe/Istanbul")

	week := make([]WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		week = append(week, day(dow, true, "11:00", "22:00"))
	}

	// 06:30 UTC = 11:30 in Almaty (open) but 09:30 in Istanbul (still shut).
	instant := time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

	if !IsOpenAt(week, instant, almaty) {
		t.Error("Almaty venue must read as open at 11:30 local")
	}
	if IsOpenAt(week, instant, istanbul) {
		t.Error("Istanbul venue must read as closed at 09:30 local")
	}
}

func TestBuildWeeklySchedule(t *testing.T) {
	tests := []struct {
		name  string
		hours []WorkingHours
		want  []ScheduleDay
	}{
		{
			name:  "no rows at all yields no days (unknown, not closed)",
			hours: nil,
			want:  []ScheduleDay{},
		},
		{
			name:  "closed day carries no times",
			hours: []WorkingHours{day(1, false, "", "")},
			want:  []ScheduleDay{{DayOfWeek: 1}},
		},
		{
			name:  "past-midnight day is flagged, not rewritten",
			hours: []WorkingHours{day(5, true, "11:00", "01:00")},
			want:  []ScheduleDay{{DayOfWeek: 5, IsOpen: true, OpenTime: "11:00", CloseTime: "01:00", ClosesNextDay: true}},
		},
		{
			name:  "seconds in the stored value are normalised to HH:MM",
			hours: []WorkingHours{day(2, true, "11:00:00", "22:00:00")},
			want:  []ScheduleDay{{DayOfWeek: 2, IsOpen: true, OpenTime: "11:00", CloseTime: "22:00"}},
		},
		{
			name:  "an hour past 24 wraps and is marked next-day",
			hours: []WorkingHours{day(6, true, "18:00", "25:30")},
			want:  []ScheduleDay{{DayOfWeek: 6, IsOpen: true, OpenTime: "18:00", CloseTime: "01:30", ClosesNextDay: true}},
		},
		{
			name:  "open row with no times is omitted (unknown), never downgraded to closed",
			hours: []WorkingHours{day(3, true, "", "")},
			want:  []ScheduleDay{},
		},
		{
			name:  "unparseable times are omitted too",
			hours: []WorkingHours{day(3, true, "later", "much later")},
			want:  []ScheduleDay{},
		},
		{
			name:  "rows are sorted by weekday and the first duplicate wins",
			hours: []WorkingHours{day(4, true, "12:00", "20:00"), day(0, false, "", ""), day(4, true, "09:00", "10:00")},
			want: []ScheduleDay{
				{DayOfWeek: 0},
				{DayOfWeek: 4, IsOpen: true, OpenTime: "12:00", CloseTime: "20:00"},
			},
		},
		{
			name:  "out-of-range weekday values are dropped",
			hours: []WorkingHours{day(9, true, "10:00", "11:00"), day(-1, true, "10:00", "11:00")},
			want:  []ScheduleDay{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildWeeklySchedule(tc.hours)
			if len(got) != len(tc.want) {
				t.Fatalf("days = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("day[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// BuildWeeklySchedule and OpeningWindow must agree on which row applies, or the
// guest would read hours the booking engine does not honour.
func TestScheduleAgreesWithOpeningWindow(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	hours := []WorkingHours{day(5, true, "11:00", "01:00"), day(5, true, "09:00", "10:00")}

	dayStart := time.Date(2026, 7, 24, 0, 0, 0, 0, almaty) // Friday
	open, close_, ok := OpeningWindow(hours, int(time.Friday), dayStart, almaty)
	if !ok {
		t.Fatal("expected an opening window for Friday")
	}
	if open.Format("15:04") != "11:00" || close_.Format("15:04") != "01:00" {
		t.Fatalf("window = %s..%s, want 11:00..01:00", open.Format("15:04"), close_.Format("15:04"))
	}
	if close_.Day() != dayStart.Day()+1 {
		t.Errorf("close must roll into the next day, got %v", close_)
	}
	sched := BuildWeeklySchedule(hours)
	if len(sched) != 1 || sched[0].OpenTime != "11:00" || sched[0].CloseTime != "01:00" {
		t.Errorf("schedule = %+v, want the same first-wins row", sched)
	}
}
