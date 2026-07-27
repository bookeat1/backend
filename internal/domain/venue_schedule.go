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

// OpeningWindow returns [open, close) for weekday dow on the calendar day of
// `day`, in loc. A close time that is not after the open time is treated as
// past midnight (18:00–02:00) and rolls into the next day. ok is false when the
// venue has no row for that weekday, the row says closed, or its times are not
// readable.
//
// The FIRST row for dow wins: duplicates are a data defect, and picking the
// first one keeps the answer deterministic and identical for every caller.
func OpeningWindow(hours []WorkingHours, dow int, day time.Time, loc *time.Location) (time.Time, time.Time, bool) {
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
		base := StartOfDay(day, loc)
		open := base.Add(time.Duration(openMin) * time.Minute)
		close_ := base.Add(time.Duration(closeMin) * time.Minute)
		if !close_.After(open) {
			close_ = close_.AddDate(0, 0, 1)
		}
		return open, close_, true
	}
	return time.Time{}, time.Time{}, false
}

// IsOpenAt reports whether the venue is open at the instant `at`, evaluated in
// loc — the VENUE's timezone, never the server's or the guest's.
//
// It checks today's window AND yesterday's, because yesterday's window may
// still be running: a venue open 11:00–01:00 on Friday is open at 00:30 on
// Saturday even if Saturday itself is a day off.
func IsOpenAt(hours []WorkingHours, at time.Time, loc *time.Location) bool {
	local := at.In(loc)
	today := StartOfDay(local, loc)
	for _, daysBack := range []int{0, 1} {
		day := today.AddDate(0, 0, -daysBack)
		open, close_, ok := OpeningWindow(hours, int(day.Weekday()), day, loc)
		if !ok {
			continue
		}
		if !local.Before(open) && local.Before(close_) {
			return true
		}
	}
	return false
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
