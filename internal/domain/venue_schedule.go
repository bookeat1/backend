package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file holds the ONE reading of a venue's weekly working hours
// (restaurant_working_hours). Both the booking/availability engine and the
// public catalog payload go through it, so "is this venue open at instant X"
// can never have two different answers in the same process.
//
// Conventions inherited from the table and honoured everywhere here:
//   - DayOfWeek is time.Weekday (0 = Sunday), the value stored in
//     restaurant_working_hours.day_of_week;
//   - OpenTime/CloseTime are venue-LOCAL "HH:MM" strings, never instants;
//   - a close time that is not after the open time means the venue works past
//     midnight (11:00–01:00) and the window rolls into the next calendar day.

// ScheduleDay is one weekday of a venue's regular weekly schedule in a shape a
// client can render without parsing prose. Times are "HH:MM" in the venue's own
// timezone; they are empty when IsOpen is false.
type ScheduleDay struct {
	DayOfWeek     int
	IsOpen        bool
	OpenTime      string
	CloseTime     string
	ClosesNextDay bool // 11:00–01:00: CloseTime belongs to the following day
}

// WeeklySchedule is a venue's structured opening hours plus the server-computed
// "open right now" answer.
//
// Days carries ONLY the weekdays the venue actually has a usable row for. A
// weekday that is missing (or whose row claims to be open but carries no
// readable HH:MM pair) is deliberately absent rather than reported as closed:
// the server does not know, and saying "closed" would be an invention. The
// booking engine treats such a day as producing no slots, which is the safe
// direction, but that is a booking decision, not a fact about the venue.
//
// OpenNow is nil when the venue's timezone could not be loaded on this host —
// "unknown", never a guessed false.
type WeeklySchedule struct {
	Timezone string
	Days     []ScheduleDay
	OpenNow  *bool

	// Exceptions are the venue's special days — a holiday closure, a one-off
	// change of hours — for the dates covered by [ExceptionsFrom,
	// ExceptionsUntil]. An exception BEATS the weekday row in Days for its
	// date, and the availability engine resolves it through the very same
	// helper (OpeningWindow), so a date shown as closed here sells no slots.
	//
	// ExceptionsFrom/Until are "YYYY-MM-DD" venue-local and are EMPTY when the
	// server could not compute the exceptions at all. That distinction is the
	// whole point: an empty list inside a stated window means "no special days
	// there", an absent window means "unknown" — the same contract Days already
	// follows (absent ≠ closed).
	Exceptions      []ScheduleException
	ExceptionsFrom  string
	ExceptionsUntil string
}

// PublicVenueState is the server-computed, guest-facing truth about a venue
// that the client would otherwise have to invent: when the venue is open, and
// whether it can take an online booking at all.
//
// Schedule is nil when the venue has no usable working-hours rows at all — the
// client must then say "hours unknown", not "closed".
type PublicVenueState struct {
	Schedule *WeeklySchedule
	// AcceptsOnlineBookings reports whether the booking engine could, in
	// principle, ever offer this venue a bookable slot. It is true only when
	// BOTH of the following hold:
	//   - the venue has at least one active table with capacity > 0 (the same
	//     filter usecase/bookings.loadSchedule applies), and
	//   - its weekly schedule has at least one day with readable opening hours
	//     (without them candidateStarts produces no start times on any date).
	// It is a static capability flag, NOT "there is a free table tonight".
	AcceptsOnlineBookings bool
}

// OpenNowKnown reports the venue's server-computed "open right now" answer and
// whether it is known at all. Unknown means either "no usable working-hours
// rows" (Schedule nil) or "the venue's timezone could not be loaded on this
// host" (OpenNow nil) — see WeeklySchedule.OpenNow. It is deliberately a second
// return value and not a false: a client that flattens the two states says
// "закрыто" about a venue nobody has entered hours for.
func (s *PublicVenueState) OpenNowKnown() (open bool, known bool) {
	if s == nil || s.Schedule == nil || s.Schedule.OpenNow == nil {
		return false, false
	}
	return *s.Schedule.OpenNow, true
}

// VenueStateFilter narrows a catalog query by the two facts the server COMPUTES
// per venue (PublicVenueState) instead of storing them in a column: "is it open
// right now, in its own timezone" and "can it take an online booking at all".
//
// It is deliberately NOT a field on RestaurantFilter / RestaurantSearchFilter.
// Everything on those is answered in SQL by the repository; these two cannot be
// and must not be. Open-now has to come from the very same IsOpenAt call that
// produces the public open_now field, and bookability from the same rule that
// produces accepts_online_bookings — a second implementation in SQL is how a
// catalog ends up filtering by one rule and displaying another (see
// bugs/bookeat-backend-bookability-flag-ignores-seats-mode). usecase/restaurants
// applies this filter in Go, after enrichment and before paging.
type VenueStateFilter struct {
	// OpenNow, when set, keeps only venues whose computed open_now equals it.
	// A venue with no usable hours is NOT open: it can never satisfy
	// OpenNow=true, and it does satisfy OpenNow=false. So the two values
	// partition the catalog exactly — their totals add up to the unfiltered
	// total — and "we don't know" is never published as "открыто".
	OpenNow *bool
	// AcceptsOnlineBookings, when set, keeps only venues whose computed
	// accepts_online_bookings equals it. That flag is already a definite
	// boolean (a venue with no readable schedule is not bookable), so here too
	// true and false partition the catalog.
	AcceptsOnlineBookings *bool
}

// Active reports whether the filter constrains anything at all.
func (f VenueStateFilter) Active() bool {
	return f.OpenNow != nil || f.AcceptsOnlineBookings != nil
}

// Matches reports whether a venue with the given computed state survives the
// filter. A nil state ("not computed") matches nothing: an unknown venue must
// never be silently published as if it satisfied a filter. Callers that cannot
// compute the state must refuse the request rather than rely on this — see
// usecase/restaurants.
func (f VenueStateFilter) Matches(st *PublicVenueState) bool {
	if st == nil {
		return false
	}
	if f.OpenNow != nil {
		open, _ := st.OpenNowKnown() // unknown → not open, never "open"
		if open != *f.OpenNow {
			return false
		}
	}
	if f.AcceptsOnlineBookings != nil && st.AcceptsOnlineBookings != *f.AcceptsOnlineBookings {
		return false
	}
	return true
}

// StartOfDay returns midnight of t's calendar day in loc. Built from the date
// parts (not by truncation) so DST transitions are handled by the location.
func StartOfDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// ParseClockMinutes parses "HH:MM" or "HH:MM:SS" into minutes since midnight.
// Hours up to 47 are accepted so a row that spells a past-midnight close as
// "25:00" still reads correctly.
func ParseClockMinutes(v string) (int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("%w: bad clock value %q", ErrValidation, v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("%w: bad clock value %q", ErrValidation, v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("%w: bad clock value %q", ErrValidation, v)
	}
	if h < 0 || h > 47 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: bad clock value %q", ErrValidation, v)
	}
	return h*60 + m, nil
}

// formatClockMinutes renders minutes-since-midnight as a wall-clock "HH:MM".
// Values past 24h wrap (25:00 → "01:00"); the fact that they belong to the next
// day is carried by ScheduleDay.ClosesNextDay, not by an out-of-range hour a
// client would have to special-case.
func formatClockMinutes(mins int) string {
	return fmt.Sprintf("%02d:%02d", (mins/60)%24, mins%60)
}

// OpeningWindow returns [open, close) for the calendar day of `day`, in loc,
// with the venue's special-day overrides applied. A close time that is not
// after the open time is treated as past midnight (18:00–02:00) and rolls into
// the next day. ok is false when the venue is shut that day — by an override,
// by its weekly row, or because there is nothing readable to go on.
//
// This is THE reading of "when is this venue open on this date". Both the
// availability engine and the guest-facing catalog go through it; the overrides
// argument is mandatory (pass nil only where there provably are none, e.g. a
// unit test of the weekly rules) precisely so a new caller cannot silently
// reintroduce the split where the storefront and the booking engine answered
// this question differently.
//
// Resolution order for a date:
//  1. a schedule override for that venue-local date, if any — it is
//     AUTHORITATIVE and replaces the weekly row entirely (closed all day, or a
//     different window, including on a weekday the venue is normally shut);
//  2. otherwise the weekly working-hours row for that weekday.
func OpeningWindow(hours []WorkingHours, overrides []ScheduleOverride, day time.Time, loc *time.Location) (time.Time, time.Time, bool) {
	if o, ok := FindScheduleOverride(overrides, day, loc); ok {
		return overrideWindow(o, day, loc)
	}
	return weeklyWindow(hours, int(day.In(loc).Weekday()), day, loc)
}

// overrideWindow is the window an override itself dictates for its date.
//
// A closed override yields no window. An "open with different hours" override
// whose times do not parse yields no window either: the row says this date is
// special, so falling back to the weekly hours would serve exactly the lie this
// code exists to remove, while inventing a window would be worse. The guest
// side reports such a date as unknown (it is omitted from the exceptions list),
// the engine sells nothing on it — the same asymmetry WeeklySchedule.Days
// already documents.
func overrideWindow(o ScheduleOverride, day time.Time, loc *time.Location) (time.Time, time.Time, bool) {
	if o.IsClosed || o.OpenTime == nil || o.CloseTime == nil {
		return time.Time{}, time.Time{}, false
	}
	openMin, err := ParseClockMinutes(*o.OpenTime)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	closeMin, err := ParseClockMinutes(*o.CloseTime)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	open, close_ := clockWindow(day, loc, openMin, closeMin)
	return open, close_, true
}

// clockWindow builds [open, close) on `day` in loc from two
// minutes-since-midnight values, rolling the close into the next day when it is
// not after the open (11:00–01:00). Shared by the weekly and the override path
// so a special day crosses midnight by exactly the same rule as a normal one.
func clockWindow(day time.Time, loc *time.Location, openMin, closeMin int) (time.Time, time.Time) {
	base := StartOfDay(day, loc)
	open := base.Add(time.Duration(openMin) * time.Minute)
	close_ := base.Add(time.Duration(closeMin) * time.Minute)
	if !close_.After(open) {
		close_ = close_.AddDate(0, 0, 1)
	}
	return open, close_
}

// dateKey renders a calendar date as "YYYY-MM-DD" from the parts t has in its
// OWN location — never a UTC conversion, which would shift the day for any
// venue east or west of Greenwich.
func dateKey(t time.Time) string {
	y, m, d := t.Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
}

// FindScheduleOverride returns the override that applies to the calendar day of
// `day` read in loc — the VENUE's zone, so an override for "2026-01-01" means
// that date on the venue's own clock.
//
// The stored override_date is a bare Postgres `date`: pgx hands it over as a
// midnight time.Time in UTC, so it is compared by its date PARTS, not by
// instant equality, which would be off by the venue's offset every time.
//
// The FIRST matching row wins. A unique index on (restaurant_id, override_date)
// already makes duplicates impossible in the database; if one ever appears (a
// hand-written row, a repository that forgets the index), first-wins keeps the
// engine and the storefront on the SAME row instead of letting them each pick
// their own — matching the first-wins rule the weekly hours already use.
func FindScheduleOverride(overrides []ScheduleOverride, day time.Time, loc *time.Location) (ScheduleOverride, bool) {
	if len(overrides) == 0 {
		return ScheduleOverride{}, false
	}
	want := dateKey(day.In(loc))
	for _, o := range overrides {
		if dateKey(o.Date) == want {
			return o, true
		}
	}
	return ScheduleOverride{}, false
}

// weeklyWindow returns [open, close) for weekday dow from the regular weekly
// rows. It knows nothing about overrides on purpose — OpeningWindow is the only
// caller, so the "override beats the week" rule is written exactly once.
//
// The FIRST row for dow wins: duplicates are a data defect, and picking the
// first one keeps the answer deterministic and identical for every caller.
func weeklyWindow(hours []WorkingHours, dow int, day time.Time, loc *time.Location) (time.Time, time.Time, bool) {
	for _, h := range hours {
		if h.DayOfWeek != dow {
			continue
		}
		if !h.IsOpen || h.OpenTime == nil || h.CloseTime == nil {
			return time.Time{}, time.Time{}, false
		}
		openMin, err := ParseClockMinutes(*h.OpenTime)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		closeMin, err := ParseClockMinutes(*h.CloseTime)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		open, close_ := clockWindow(day, loc, openMin, closeMin)
		return open, close_, true
	}
	return time.Time{}, time.Time{}, false
}

// IsOpenAt reports whether the venue is open at the instant `at`, evaluated in
// loc — the VENUE's timezone, never the server's or the guest's — with the
// special-day overrides applied.
//
// It checks today's window AND yesterday's, because yesterday's window may
// still be running: a venue open 11:00–01:00 on Friday is open at 00:30 on
// Saturday even if Saturday itself is a day off.
//
// Note what that means for a closure: an override closes a DATE, i.e. the shift
// that STARTS on that date. A venue open 11:00–02:00 on 31 December and closed
// by an override on 1 January is still open at 00:30 on 1 January — the New
// Year's shift that began the previous evening is running — and shut from 02:00
// onwards. Any other reading would require the override to reach back and
// truncate a window it says nothing about.
func IsOpenAt(hours []WorkingHours, overrides []ScheduleOverride, at time.Time, loc *time.Location) bool {
	local := at.In(loc)
	today := StartOfDay(local, loc)
	for _, daysBack := range []int{0, 1} {
		day := today.AddDate(0, 0, -daysBack)
		open, close_, ok := OpeningWindow(hours, overrides, day, loc)
		if !ok {
			continue
		}
		if !local.Before(open) && local.Before(close_) {
			return true
		}
	}
	return false
}

// ScheduleException is ONE dated departure from the weekly schedule, in the
// same shape a client already renders a weekday: a date the venue is shut, or a
// date it works different hours (including a date it opens although the weekly
// schedule says that weekday is off).
//
// Date is "YYYY-MM-DD" in the venue's own timezone. Times are "HH:MM" venue
// local and are empty when IsOpen is false.
type ScheduleException struct {
	Date          string
	IsOpen        bool
	OpenTime      string
	CloseTime     string
	ClosesNextDay bool
	Note          string
}

// BuildScheduleExceptions renders the overrides that fall inside the window
// [from, from+days) — venue-local dates — as a client-renderable list, ordered
// by date.
//
// Overrides OUTSIDE that window are dropped, in particular past ones: an
// override for a date that has already gone by says nothing about today, and
// shipping it would invite a client to apply a stale closure.
//
// An override that claims different hours but carries times nobody can parse is
// omitted rather than reported as closed — a malformed row is unknown data, and
// "closed" would be an invention (WeeklySchedule.Days takes the same line).
// The booking engine independently sells nothing on such a date; that is the
// safe direction for money, not a claim about the venue.
func BuildScheduleExceptions(overrides []ScheduleOverride, from time.Time, days int, loc *time.Location) []ScheduleException {
	out := make([]ScheduleException, 0, len(overrides))
	if days <= 0 {
		return out
	}
	start := StartOfDay(from, loc)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i)
		o, ok := FindScheduleOverride(overrides, day, loc)
		if !ok {
			continue
		}
		ex := ScheduleException{Date: dateKey(day)}
		if o.Note != nil {
			ex.Note = *o.Note
		}
		if o.IsClosed {
			out = append(out, ex)
			continue
		}
		open, close_, ok := overrideWindow(o, day, loc)
		if !ok {
			continue // "special, but unreadable" — unknown, never guessed
		}
		ex.IsOpen = true
		ex.OpenTime = open.Format("15:04")
		ex.CloseTime = close_.Format("15:04")
		ex.ClosesNextDay = dateKey(close_) != ex.Date
		out = append(out, ex)
	}
	return out
}

// BuildWeeklySchedule renders the stored rows as a client-renderable week,
// ordered by weekday (Sunday first, matching day_of_week).
//
// A row flagged open but missing a readable HH:MM pair is OMITTED rather than
// downgraded to "closed" — see WeeklySchedule.Days. Rows for the same weekday
// follow OpeningWindow's first-wins rule so the schedule and the availability
// engine can never disagree about which row applies.
func BuildWeeklySchedule(hours []WorkingHours) []ScheduleDay {
	seen := make(map[int]bool, len(hours))
	out := make([]ScheduleDay, 0, len(hours))
	for _, h := range hours {
		if h.DayOfWeek < 0 || h.DayOfWeek > 6 || seen[h.DayOfWeek] {
			continue
		}
		seen[h.DayOfWeek] = true

		if !h.IsOpen {
			out = append(out, ScheduleDay{DayOfWeek: h.DayOfWeek})
			continue
		}
		if h.OpenTime == nil || h.CloseTime == nil {
			continue // open, but the hours are not recorded — unknown
		}
		openMin, err := ParseClockMinutes(*h.OpenTime)
		if err != nil {
			continue
		}
		closeMin, err := ParseClockMinutes(*h.CloseTime)
		if err != nil {
			continue
		}
		out = append(out, ScheduleDay{
			DayOfWeek:     h.DayOfWeek,
			IsOpen:        true,
			OpenTime:      formatClockMinutes(openMin),
			CloseTime:     formatClockMinutes(closeMin),
			ClosesNextDay: closeMin <= openMin || closeMin >= 24*60,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DayOfWeek < out[j].DayOfWeek })
	return out
}
