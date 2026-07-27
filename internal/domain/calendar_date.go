package domain

import (
	"fmt"
	"strings"
	"time"
)

// CalendarDate is a day on a wall calendar: no time of day, no zone, and
// therefore not an instant. "2026-07-25" at a venue in Almaty and the same
// string at a venue in Lisbon name two different five-hour-apart windows on the
// world clock, and only the venue's own zone can turn one into the other.
//
// It exists so a filter can travel from the transport layer to the usecase
// UNRESOLVED. The alternative — parsing "2026-07-25" into a time.Time at the
// edge — silently produces UTC midnight, which reads like a real instant, gets
// compared against real instants, and is wrong by the venue's offset for every
// venue outside UTC.
type CalendarDate struct {
	Year  int
	Month time.Month
	Day   int
}

// CalendarDateLayout is the only accepted form of a calendar date on the wire.
const CalendarDateLayout = "2006-01-02"

// ParseCalendarDate reads "YYYY-MM-DD". It rejects anything else, including a
// full timestamp: a caller that knows the instant it means should send an
// instant (from/to), not a date.
func ParseCalendarDate(s string) (CalendarDate, error) {
	t, err := time.Parse(CalendarDateLayout, strings.TrimSpace(s))
	if err != nil {
		return CalendarDate{}, fmt.Errorf("%w: date must be YYYY-MM-DD", ErrValidation)
	}
	return CalendarDate{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// Bounds returns the half-open instant window [from, to) this calendar day
// occupies in loc.
//
// Both ends go through time.Date, which normalises the calendar fields BEFORE
// resolving the zone offset, so the window is exactly "from this day's local
// midnight to the next day's local midnight" — 23 or 25 hours long on a
// daylight-saving transition, as the day really is. Adding 24h to the start
// instead would slice an hour off one day of the year and double an hour of
// another, which is the same mistake OpeningWindow had (see venue_schedule.go).
func (d CalendarDate) Bounds(loc *time.Location) (from, to time.Time) {
	from = time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
	to = time.Date(d.Year, d.Month, d.Day+1, 0, 0, 0, 0, loc)
	return from, to
}

// String renders the date back in the wire format.
func (d CalendarDate) String() string {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Format(CalendarDateLayout)
}
