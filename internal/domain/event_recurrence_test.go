package domain

import (
	"testing"
	"time"
)

// weeklyRule is "every Wednesday and Thursday at 19:00, three hours long",
// exactly the shape of «Cocktail Wednesday» / «Караоке-битва по четвергам».
func weeklyRule(startsOn CalendarDate) EventRecurrence {
	return EventRecurrence{
		Frequency:       RecurrenceWeekly,
		Weekdays:        []ISOWeekday{3, 4}, // Wed, Thu
		StartMinutes:    19 * 60,
		DurationMinutes: 180,
		StartsOn:        startsOn,
	}
}

func fmtLocal(ts []time.Time, loc *time.Location) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.In(loc).Format("2006-01-02 15:04 -0700"))
	}
	return out
}

func assertSlots(t *testing.T, got []time.Time, loc *time.Location, want ...string) {
	t.Helper()
	gotStr := fmtLocal(got, loc)
	if len(gotStr) != len(want) {
		t.Fatalf("slots: got %v, want %v", gotStr, want)
	}
	for i := range want {
		if gotStr[i] != want[i] {
			t.Fatalf("slot %d: got %q, want %q (all: %v)", i, gotStr[i], want[i], gotStr)
		}
	}
}

func TestOccurrencesWeeklyTwoWeekdays(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 17}) // Monday
	// Window: Mon 17 Aug 00:00 → Mon 31 Aug 00:00 local, i.e. exactly two weeks.
	from := time.Date(2026, time.August, 17, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.August, 31, 0, 0, 0, 0, loc)

	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-08-19 19:00 +0500", // Wed
		"2026-08-20 19:00 +0500", // Thu
		"2026-08-26 19:00 +0500", // Wed
		"2026-08-27 19:00 +0500", // Thu
	)
}

func TestOccurrencesDaily(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := EventRecurrence{
		Frequency:       RecurrenceDaily,
		StartMinutes:    12*60 + 30,
		DurationMinutes: 90,
		StartsOn:        CalendarDate{2026, time.August, 17},
	}
	from := time.Date(2026, time.August, 17, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.August, 21, 0, 0, 0, 0, loc)

	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-08-17 12:30 +0500",
		"2026-08-18 12:30 +0500",
		"2026-08-19 12:30 +0500",
		"2026-08-20 12:30 +0500",
	)
}

func TestOccurrencesMonthlySkipsMonthsWithoutThatDay(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	day := 31
	r := EventRecurrence{
		Frequency:       RecurrenceMonthly,
		MonthDay:        &day,
		StartMinutes:    20 * 60,
		DurationMinutes: 120,
		StartsOn:        CalendarDate{2026, time.January, 1},
	}
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.May, 1, 0, 0, 0, 0, loc)

	// February (28 days) and April (30 days) simply produce nothing — the event
	// is NOT slid onto the 28th/30th.
	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-01-31 20:00 +0500",
		"2026-03-31 20:00 +0500",
	)
}

// The reason the whole rule stores wall-clock minutes and resolves them through
// time.Date: on a DST transition the local time must NOT move. Europe/Lisbon is
// used because it actually transitions; Asia/Almaty (below) does not, which is
// precisely why an Almaty-only test could never catch the bug class.
func TestOccurrencesKeepWallClockAcrossDSTTransition(t *testing.T) {
	loc := mustLoad(t, "Europe/Lisbon")
	r := weeklyRule(CalendarDate{2026, time.October, 19})
	from := time.Date(2026, time.October, 19, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.November, 2, 0, 0, 0, 0, loc)

	// Lisbon goes back to winter time on 2026-10-25. Every occurrence must stay
	// at 19:00 on the wall; the UTC OFFSET is what changes, not the local hour.
	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-10-21 19:00 +0100",
		"2026-10-22 19:00 +0100",
		"2026-10-28 19:00 +0000",
		"2026-10-29 19:00 +0000",
	)
}

// Asia/Almaty is a fixed +05:00 zone today (it dropped DST and moved off +06:00
// in 2024). This test pins the instants a venue in Almaty really gets, so a
// future refactor that reintroduces "midnight + N minutes" or resolves the slot
// in UTC fails here loudly.
func TestOccurrencesAlmatyInstantsAreVenueLocal(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 17})
	from := time.Date(2026, time.August, 17, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.August, 24, 0, 0, 0, 0, loc)

	got := r.Occurrences(loc, from, to)
	if len(got) != 2 {
		t.Fatalf("want 2 slots, got %v", fmtLocal(got, loc))
	}
	// 19:00 in Almaty is 14:00 UTC — NOT 19:00 UTC, which is what a
	// zone-unaware implementation would produce.
	if utc := got[0].UTC().Format("2006-01-02T15:04:05Z"); utc != "2026-08-19T14:00:00Z" {
		t.Fatalf("first slot in UTC = %s, want 2026-08-19T14:00:00Z", utc)
	}
	if end := r.EndOf(got[0]).In(loc).Format("15:04"); end != "22:00" {
		t.Fatalf("end of first slot = %s local, want 22:00", end)
	}
}

func TestOccurrencesStopAtUntilDateInclusive(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 17})
	until := CalendarDate{2026, time.August, 26} // a Wednesday: that day still fires
	r.UntilDate = &until
	from := time.Date(2026, time.August, 17, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.September, 30, 0, 0, 0, 0, loc)

	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-08-19 19:00 +0500",
		"2026-08-20 19:00 +0500",
		"2026-08-26 19:00 +0500",
	)
}

func TestOccurrencesRespectWindowBoundaries(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 1})

	// `from` is INCLUSIVE: a slot exactly at from is produced.
	from := time.Date(2026, time.August, 19, 19, 0, 0, 0, loc)
	// `to` is EXCLUSIVE: the slot exactly at to is NOT.
	to := time.Date(2026, time.August, 26, 19, 0, 0, 0, loc)
	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-08-19 19:00 +0500",
		"2026-08-20 19:00 +0500",
	)

	// One minute later on both ends: the first slot falls out, the last falls in.
	assertSlots(t, r.Occurrences(loc, from.Add(time.Minute), to.Add(time.Minute)), loc,
		"2026-08-20 19:00 +0500",
		"2026-08-26 19:00 +0500",
	)
}

func TestOccurrencesNeverStartBeforeStartsOn(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 25}) // rule begins mid-window
	from := time.Date(2026, time.August, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.September, 1, 0, 0, 0, 0, loc)

	assertSlots(t, r.Occurrences(loc, from, to), loc,
		"2026-08-26 19:00 +0500",
		"2026-08-27 19:00 +0500",
	)
}

func TestOccurrencesEmptyForDegenerateInput(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	r := weeklyRule(CalendarDate{2026, time.August, 17})
	from := time.Date(2026, time.August, 17, 0, 0, 0, 0, loc)

	if got := r.Occurrences(loc, from, from); got != nil {
		t.Fatalf("empty window must produce nothing, got %v", fmtLocal(got, loc))
	}
	if got := r.Occurrences(nil, from, from.Add(time.Hour)); got != nil {
		t.Fatalf("nil location must produce nothing, got %d slots", len(got))
	}
	// An until-date before the rule's start can only mean an empty series.
	past := CalendarDate{2026, time.August, 1}
	r.UntilDate = &past
	if got := r.Occurrences(loc, from, from.AddDate(0, 1, 0)); got != nil {
		t.Fatalf("until before starts_on must produce nothing, got %v", fmtLocal(got, loc))
	}
}

func TestISOWeekdayOf(t *testing.T) {
	for _, tc := range []struct {
		in   time.Weekday
		want ISOWeekday
	}{
		{time.Monday, 1}, {time.Wednesday, 3}, {time.Saturday, 6}, {time.Sunday, 7},
	} {
		if got := ISOWeekdayOf(tc.in); got != tc.want {
			t.Fatalf("ISOWeekdayOf(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRecurrenceFrequencyValid(t *testing.T) {
	for _, f := range []RecurrenceFrequency{RecurrenceDaily, RecurrenceWeekly, RecurrenceMonthly} {
		if !f.Valid() {
			t.Fatalf("%q must be valid", f)
		}
	}
	for _, f := range []RecurrenceFrequency{"", "yearly", "Weekly"} {
		if f.Valid() {
			t.Fatalf("%q must not be valid", f)
		}
	}
}
