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
			if got := IsOpenAt(week, nil, tc.local, almaty); got != tc.want {
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

	if !IsOpenAt(week, nil, instant, almaty) {
		t.Error("Almaty venue must read as open at 11:30 local")
	}
	if IsOpenAt(week, nil, instant, istanbul) {
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
	open, close_, ok := OpeningWindow(hours, nil, dayStart, almaty)
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

// ---------------------------------------------------------------------------
// Special-day overrides (restaurant_schedule_overrides).
//
// These pin the ONE rule both the storefront and the availability engine now
// read a date through: an override for a venue-local date replaces that date's
// weekly row entirely.
// ---------------------------------------------------------------------------

// closedOn builds a "shut all day" override for a date, in the shape the
// database stores it: a bare date, handed over by pgx as UTC midnight.
func closedOn(date string) ScheduleOverride {
	return ScheduleOverride{Date: mustDate(date), IsClosed: true}
}

// hoursOn builds an "open, but different hours" override.
func hoursOn(date, open, close_ string) ScheduleOverride {
	return ScheduleOverride{Date: mustDate(date), OpenTime: hoursp(open), CloseTime: hoursp(close_)}
}

func mustDate(date string) time.Time {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return t
}

// The whole point of the change: for a given date the override wins, in both
// directions (it can shut an open day AND open a shut one), and it is matched
// against the VENUE's calendar date.
func TestOpeningWindowAppliesOverrides(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")

	// 2026-01-01 is a Thursday, 2026-01-02 a Friday, 2026-01-03 a Saturday.
	week := []WorkingHours{
		day(int(time.Thursday), true, "11:00", "22:00"),
		day(int(time.Friday), true, "11:00", "22:00"),
		day(int(time.Saturday), false, "", ""), // normally shut
	}

	tests := []struct {
		name      string
		overrides []ScheduleOverride
		date      string
		wantOK    bool
		wantOpen  string // "HH:MM" venue-local
		wantClose string
		wantNext  bool // the close belongs to the following calendar day
	}{
		{
			name: "no override: the weekly row still decides",
			date: "2026-01-01", wantOK: true, wantOpen: "11:00", wantClose: "22:00",
		},
		{
			name:      "an override closes an otherwise open day",
			overrides: []ScheduleOverride{closedOn("2026-01-01")},
			date:      "2026-01-01", wantOK: false,
		},
		{
			name:      "an override replaces the hours of an open day",
			overrides: []ScheduleOverride{hoursOn("2026-01-01", "16:00", "20:00")},
			date:      "2026-01-01", wantOK: true, wantOpen: "16:00", wantClose: "20:00",
		},
		{
			name:      "an override opens a day the venue is normally closed",
			overrides: []ScheduleOverride{hoursOn("2026-01-03", "12:00", "18:00")},
			date:      "2026-01-03", wantOK: true, wantOpen: "12:00", wantClose: "18:00",
		},
		{
			name:      "an override may cross midnight, like any other day",
			overrides: []ScheduleOverride{hoursOn("2026-01-01", "18:00", "02:00")},
			date:      "2026-01-01", wantOK: true, wantOpen: "18:00", wantClose: "02:00", wantNext: true,
		},
		{
			name:      "an override for another date leaves this one alone",
			overrides: []ScheduleOverride{closedOn("2026-01-02")},
			date:      "2026-01-01", wantOK: true, wantOpen: "11:00", wantClose: "22:00",
		},
		{
			name:      "a PAST override does not reach today",
			overrides: []ScheduleOverride{closedOn("2025-12-25")},
			date:      "2026-01-01", wantOK: true, wantOpen: "11:00", wantClose: "22:00",
		},
		{
			name: "duplicates for one date: the first row wins, for everyone",
			overrides: []ScheduleOverride{
				hoursOn("2026-01-01", "16:00", "20:00"),
				hoursOn("2026-01-01", "09:00", "10:00"),
				closedOn("2026-01-01"),
			},
			date: "2026-01-01", wantOK: true, wantOpen: "16:00", wantClose: "20:00",
		},
		{
			name:      "an unreadable override does NOT fall back to the weekly hours",
			overrides: []ScheduleOverride{hoursOn("2026-01-01", "later", "much later")},
			date:      "2026-01-01", wantOK: false,
		},
		{
			name:      "an override missing its times is not a window either",
			overrides: []ScheduleOverride{{Date: mustDate("2026-01-01")}},
			date:      "2026-01-01", wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := time.Date(mustDate(tc.date).Year(), mustDate(tc.date).Month(), mustDate(tc.date).Day(),
				0, 0, 0, 0, almaty)
			open, close_, ok := OpeningWindow(week, tc.overrides, d, almaty)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (window %v..%v)", ok, tc.wantOK, open, close_)
			}
			if !ok {
				return
			}
			if got := open.Format("15:04"); got != tc.wantOpen {
				t.Errorf("open = %s, want %s", got, tc.wantOpen)
			}
			if got := close_.Format("15:04"); got != tc.wantClose {
				t.Errorf("close = %s, want %s", got, tc.wantClose)
			}
			if next := close_.Day() != open.Day(); next != tc.wantNext {
				t.Errorf("closes next day = %v, want %v", next, tc.wantNext)
			}
			if open.Location() != almaty || close_.Location() != almaty {
				t.Errorf("window must be in the venue zone, got %v..%v", open.Location(), close_.Location())
			}
		})
	}
}

// The override date is a VENUE-local calendar date. The same override must
// apply to different instants for two venues in different zones — and the
// stored UTC-midnight date must not slide a day for a venue east of Greenwich.
func TestOverrideDateIsResolvedInTheVenueZone(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")       // UTC+5
	istanbul := mustLoad(t, "Europe/Istanbul") // UTC+3

	week := make([]WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		week = append(week, day(dow, true, "11:00", "22:00"))
	}
	overrides := []ScheduleOverride{closedOn("2026-01-01")}

	// 2025-12-31 20:00 UTC is already 2026-01-01 01:00 in Almaty (closed by the
	// override) but still 2025-12-31 23:00 in Istanbul — a normal day there.
	instant := time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC)

	if _, _, ok := OpeningWindow(week, overrides, instant, almaty); ok {
		t.Error("Almaty venue: 1 January is closed by the override")
	}
	if _, _, ok := OpeningWindow(week, overrides, instant, istanbul); !ok {
		t.Error("Istanbul venue: it is still 31 December there, a normal day")
	}
}

func TestIsOpenAtHonoursOverrides(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")

	week := make([]WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		week = append(week, day(dow, true, "11:00", "22:00"))
	}

	tests := []struct {
		name      string
		overrides []ScheduleOverride
		at        time.Time
		want      bool
	}{
		{
			name: "no override: the weekly hours answer",
			at:   time.Date(2026, 1, 1, 13, 0, 0, 0, almaty), want: true,
		},
		{
			name:      "a holiday closure shuts the venue mid-day",
			overrides: []ScheduleOverride{closedOn("2026-01-01")},
			at:        time.Date(2026, 1, 1, 13, 0, 0, 0, almaty), want: false,
		},
		{
			name:      "changed hours are honoured: before the special opening",
			overrides: []ScheduleOverride{hoursOn("2026-01-01", "16:00", "20:00")},
			at:        time.Date(2026, 1, 1, 13, 0, 0, 0, almaty), want: false,
		},
		{
			name:      "changed hours are honoured: inside the special window",
			overrides: []ScheduleOverride{hoursOn("2026-01-01", "16:00", "20:00")},
			at:        time.Date(2026, 1, 1, 17, 0, 0, 0, almaty), want: true,
		},
		{
			name:      "yesterday's special late shift is still running at 00:30",
			overrides: []ScheduleOverride{hoursOn("2025-12-31", "18:00", "02:00")},
			at:        time.Date(2026, 1, 1, 0, 30, 0, 0, almaty), want: true,
		},
		{
			name: "a closure on the date does not truncate the previous evening's shift",
			overrides: []ScheduleOverride{
				hoursOn("2025-12-31", "18:00", "02:00"), closedOn("2026-01-01"),
			},
			at: time.Date(2026, 1, 1, 0, 30, 0, 0, almaty), want: true,
		},
		{
			name: "...but the closed date itself is shut once that shift ends",
			overrides: []ScheduleOverride{
				hoursOn("2025-12-31", "18:00", "02:00"), closedOn("2026-01-01"),
			},
			at: time.Date(2026, 1, 1, 13, 0, 0, 0, almaty), want: false,
		},
		{
			name:      "a past override says nothing about today",
			overrides: []ScheduleOverride{closedOn("2025-12-25")},
			at:        time.Date(2026, 1, 1, 13, 0, 0, 0, almaty), want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOpenAt(week, tc.overrides, tc.at, almaty); got != tc.want {
				t.Errorf("IsOpenAt(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestBuildScheduleExceptions(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	from := time.Date(2026, 1, 1, 9, 30, 0, 0, almaty) // mid-day: the day, not the instant, matters

	note := "Новогодние каникулы"
	tests := []struct {
		name      string
		overrides []ScheduleOverride
		days      int
		want      []ScheduleException
	}{
		{
			name: "no overrides yields an empty list, never a nil claim",
			days: 7, want: []ScheduleException{},
		},
		{
			name:      "a closure carries no times",
			overrides: []ScheduleOverride{closedOn("2026-01-02")},
			days:      7,
			want:      []ScheduleException{{Date: "2026-01-02"}},
		},
		{
			name:      "the note the venue wrote is passed through",
			overrides: []ScheduleOverride{{Date: mustDate("2026-01-02"), IsClosed: true, Note: &note}},
			days:      7,
			want:      []ScheduleException{{Date: "2026-01-02", Note: note}},
		},
		{
			name:      "special hours are rendered like a weekday",
			overrides: []ScheduleOverride{hoursOn("2026-01-02", "16:00", "20:00")},
			days:      7,
			want:      []ScheduleException{{Date: "2026-01-02", IsOpen: true, OpenTime: "16:00", CloseTime: "20:00"}},
		},
		{
			name:      "a special day crossing midnight is flagged, not rewritten",
			overrides: []ScheduleOverride{hoursOn("2026-01-02", "18:00", "02:00")},
			days:      7,
			want: []ScheduleException{
				{Date: "2026-01-02", IsOpen: true, OpenTime: "18:00", CloseTime: "02:00", ClosesNextDay: true},
			},
		},
		{
			name:      "today itself is inside the window",
			overrides: []ScheduleOverride{closedOn("2026-01-01")},
			days:      7,
			want:      []ScheduleException{{Date: "2026-01-01"}},
		},
		{
			name:      "a past override is dropped: it says nothing about the days ahead",
			overrides: []ScheduleOverride{closedOn("2025-12-31")},
			days:      7, want: []ScheduleException{},
		},
		{
			name:      "an override past the horizon is dropped too",
			overrides: []ScheduleOverride{closedOn("2026-01-08")},
			days:      7, want: []ScheduleException{},
		},
		{
			name:      "the last covered day is inclusive",
			overrides: []ScheduleOverride{closedOn("2026-01-07")},
			days:      7,
			want:      []ScheduleException{{Date: "2026-01-07"}},
		},
		{
			name: "rows come back in date order whatever order they arrived in",
			overrides: []ScheduleOverride{
				closedOn("2026-01-05"), hoursOn("2026-01-02", "16:00", "20:00"), closedOn("2026-01-03"),
			},
			days: 7,
			want: []ScheduleException{
				{Date: "2026-01-02", IsOpen: true, OpenTime: "16:00", CloseTime: "20:00"},
				{Date: "2026-01-03"},
				{Date: "2026-01-05"},
			},
		},
		{
			name: "duplicates collapse to the same first-wins row the engine uses",
			overrides: []ScheduleOverride{
				hoursOn("2026-01-02", "16:00", "20:00"), closedOn("2026-01-02"),
			},
			days: 7,
			want: []ScheduleException{{Date: "2026-01-02", IsOpen: true, OpenTime: "16:00", CloseTime: "20:00"}},
		},
		{
			name:      "an unreadable override is omitted (unknown), never shown as closed",
			overrides: []ScheduleOverride{hoursOn("2026-01-02", "nope", "nope")},
			days:      7, want: []ScheduleException{},
		},
		{
			name:      "a zero-length window covers nothing",
			overrides: []ScheduleOverride{closedOn("2026-01-01")},
			days:      0, want: []ScheduleException{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildScheduleExceptions(tc.overrides, from, tc.days, almaty)
			if len(got) != len(tc.want) {
				t.Fatalf("exceptions = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("exception[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The guest-facing exception list and the window the engine sells inside must
// come from the same reading of the same row — that is the drift this change
// exists to remove.
func TestExceptionsAgreeWithTheOpeningWindow(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	week := []WorkingHours{day(int(time.Thursday), true, "11:00", "22:00")}
	overrides := []ScheduleOverride{hoursOn("2026-01-01", "18:00", "02:00"), closedOn("2026-01-08")}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, almaty)

	ex := BuildScheduleExceptions(overrides, from, 14, almaty)
	if len(ex) != 2 {
		t.Fatalf("exceptions = %+v, want two", ex)
	}
	open, close_, ok := OpeningWindow(week, overrides, from, almaty)
	if !ok || open.Format("15:04") != ex[0].OpenTime || close_.Format("15:04") != ex[0].CloseTime {
		t.Errorf("engine window %v..%v disagrees with the published %s..%s",
			open, close_, ex[0].OpenTime, ex[0].CloseTime)
	}
	closedDay := time.Date(2026, 1, 8, 0, 0, 0, 0, almaty)
	if _, _, ok := OpeningWindow(week, overrides, closedDay, almaty); ok {
		t.Error("a date published as closed must have no bookable window")
	}
	if ex[1].IsOpen {
		t.Error("the published exception for that date must say closed")
	}
}
