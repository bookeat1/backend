package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RecurrenceFrequency is how often a rule repeats, stored as VARCHAR (validated
// here, never a Postgres ENUM — same convention as every other enumerated field
// in this codebase).
type RecurrenceFrequency string

const (
	// RecurrenceDaily produces one occurrence every calendar day.
	RecurrenceDaily RecurrenceFrequency = "daily"
	// RecurrenceWeekly produces one occurrence on each listed weekday.
	RecurrenceWeekly RecurrenceFrequency = "weekly"
	// RecurrenceMonthly produces one occurrence on a fixed day of the month.
	RecurrenceMonthly RecurrenceFrequency = "monthly"
)

// Valid reports whether f is a known frequency.
func (f RecurrenceFrequency) Valid() bool {
	switch f {
	case RecurrenceDaily, RecurrenceWeekly, RecurrenceMonthly:
		return true
	}
	return false
}

// ISOWeekday is a weekday in ISO form: 1 = Monday … 7 = Sunday. That is the
// form Postgres (isodow), the API payload and the cabinet all speak; Go's
// time.Weekday, which starts at Sunday = 0, is converted at the boundary by
// ISOWeekdayOf so the two numbering schemes never meet in the middle of a
// calculation.
type ISOWeekday int

// Valid reports whether d names a real weekday.
func (d ISOWeekday) Valid() bool { return d >= 1 && d <= 7 }

// ISOWeekdayOf converts a Go weekday to the ISO numbering.
func ISOWeekdayOf(w time.Weekday) ISOWeekday {
	if w == time.Sunday {
		return 7
	}
	return ISOWeekday(w)
}

// EventRecurrence is a rule that keeps producing events: "Cocktail Wednesday",
// "Караоке-битва по четвергам". It is NOT an event and is never shown to a
// guest — the worker (usecase/eventrecurrence.Generator) materialises it into
// real rows of the `events` table for a rolling window ahead, and those rows are
// what the Афиша, the ticketing and the feed read.
//
// Everything from Title to TicketRefundCutoffMinutes is the TEMPLATE: it is
// copied verbatim onto each occurrence at generation time. Changing the
// template changes what FUTURE occurrences look like; occurrences that already
// exist are left exactly as they are, including any edit the venue made to a
// single date. That asymmetry is the point — see the Generator's doc.
//
// The rule stores wall-clock time (StartMinutes + a zone), never an instant.
// "Every Wednesday at 19:00" means 19:00 on the wall in the venue's zone, on
// both sides of a daylight-saving transition.
type EventRecurrence struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID

	// --- event template ---
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	// Venue is free-text location detail within the restaurant, same meaning as
	// Event.Venue ("rooftop terrace").
	Venue         string
	CoverImageURL *string
	Tags          []string
	// OccurrenceStatus is the status every generated occurrence is born with.
	// Defaults to EventDraft, the same conservative default as a hand-made
	// event: a rule does not publish unreviewed content merely by existing.
	OccurrenceStatus EventStatus
	Ticketed         bool
	// TicketPriceMinor is integer minor units, never a float.
	TicketPriceMinor *int64
	// Capacity is PER OCCURRENCE: "20 seats every Wednesday" means 20 seats on
	// each Wednesday, not 20 across the whole series.
	Capacity                  *int
	TicketsRefundable         bool
	TicketRefundCutoffMinutes int

	// --- the rule ---
	Frequency RecurrenceFrequency
	// Weekdays is meaningful only for RecurrenceWeekly (and required there).
	Weekdays []ISOWeekday
	// MonthDay (1..31) is meaningful only for RecurrenceMonthly (and required
	// there). A month without that day produces nothing that month.
	MonthDay *int
	// StartMinutes is the local start time as minutes since local midnight
	// (0..1439). An integer rather than a time.Time so it can never be mistaken
	// for an instant.
	StartMinutes int
	// DurationMinutes is the absolute length of one occurrence.
	DurationMinutes int
	// Timezone OVERRIDES the venue's zone for this rule. Empty means "follow the
	// venue" (restaurants.timezone), and only if the venue has none does the
	// platform fallback apply. Never store "" as a zone: time.LoadLocation("")
	// silently returns UTC — see NormalizeVenueTimezone.
	Timezone string
	// StartsOn is the first calendar day the rule may produce on; UntilDate, if
	// set, is the last one INCLUSIVE.
	StartsOn  CalendarDate
	UntilDate *CalendarDate
	// IsActive false stops FUTURE generation. It never removes occurrences that
	// already exist.
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ActiveEventRecurrence is one row of the generator's scan: an active rule plus
// the zone its venue is in, joined in so the worker resolves the zone without a
// second query per rule (the same join-it-in choice the feed read model makes).
// VenueTimezone is empty when the venue carries no zone of its own.
type ActiveEventRecurrence struct {
	EventRecurrence
	VenueTimezone string
}

// maxOccurrenceScanDays bounds the day-by-day scan in Occurrences. A caller
// asking for a window wider than this is a bug (the worker's window is weeks,
// not years), and an unbounded loop over a bad window would be a silent hang.
const maxOccurrenceScanDays = 800

// Occurrences returns the slot start instants this rule produces inside the
// half-open window [from, to), in chronological order, resolved in loc.
//
// Timezone correctness is the whole job of this function, so it is worth being
// explicit about how it is achieved:
//
//   - it walks CALENDAR DAYS, not 24-hour steps. A day is 23 or 25 hours long
//     on a daylight-saving transition, so stepping by 24h would drift the
//     series by an hour and eventually skip or double a date;
//   - each slot is built with time.Date(y, m, d, 0, StartMinutes, 0, 0, loc),
//     which normalises the calendar fields BEFORE resolving the zone offset.
//     That yields the WALL-CLOCK time the venue wrote down. Building it as
//     "local midnight + N minutes" is the bug fixed on 2026-07-27 in
//     venue_schedule.go: on a transition date it moves the event by an hour.
//
// StartsOn/UntilDate bound the series by CALENDAR DAY in loc (UntilDate
// inclusive), while from/to bound it by INSTANT — the two are different
// questions and both are asked.
func (r EventRecurrence) Occurrences(loc *time.Location, from, to time.Time) []time.Time {
	if loc == nil || !to.After(from) {
		return nil
	}
	// Start the day-scan at the later of "the rule's first day" and "the day
	// `from` falls on in loc" — never before either.
	day := r.StartsOn
	if d := dateIn(from, loc); dateAfter(d, day) {
		day = d
	}
	last := dateIn(to, loc) // `to` is exclusive, but a slot earlier that day may still qualify
	if r.UntilDate != nil && dateAfter(last, *r.UntilDate) {
		last = *r.UntilDate
	}

	var out []time.Time
	for scanned := 0; !dateAfter(day, last) && scanned <= maxOccurrenceScanDays; scanned++ {
		if r.matchesDay(day, loc) {
			slot := time.Date(day.Year, day.Month, day.Day, 0, r.StartMinutes, 0, 0, loc)
			if !slot.Before(from) && slot.Before(to) {
				out = append(out, slot)
			}
		}
		day = nextDay(day)
	}
	return out
}

// matchesDay answers whether the rule fires on this calendar day.
func (r EventRecurrence) matchesDay(d CalendarDate, loc *time.Location) bool {
	switch r.Frequency {
	case RecurrenceDaily:
		return true
	case RecurrenceWeekly:
		want := ISOWeekdayOf(time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, loc).Weekday())
		for _, w := range r.Weekdays {
			if w == want {
				return true
			}
		}
		return false
	case RecurrenceMonthly:
		// A month that has no such day (the 31st of February) produces nothing:
		// sliding the event to the 28th would invent a date the venue never
		// asked for, and doing it silently is worse than a missing week.
		return r.MonthDay != nil && d.Day == *r.MonthDay
	}
	return false
}

// EndOf returns the end instant of a slot. The duration is ABSOLUTE ("this
// party lasts three hours"), so it is added as elapsed time — unlike the START,
// which is wall-clock.
func (r EventRecurrence) EndOf(slot time.Time) time.Time {
	return slot.Add(time.Duration(r.DurationMinutes) * time.Minute)
}

// dateIn is the calendar day an instant falls on in loc.
func dateIn(t time.Time, loc *time.Location) CalendarDate {
	l := t.In(loc)
	return CalendarDate{Year: l.Year(), Month: l.Month(), Day: l.Day()}
}

// nextDay advances a calendar date by one day, normalising month/year through
// time.Date (in UTC — this is pure calendar arithmetic, no zone involved).
func nextDay(d CalendarDate) CalendarDate {
	t := time.Date(d.Year, d.Month, d.Day+1, 0, 0, 0, 0, time.UTC)
	return CalendarDate{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// dateAfter reports whether a is a later calendar day than b.
func dateAfter(a, b CalendarDate) bool {
	if a.Year != b.Year {
		return a.Year > b.Year
	}
	if a.Month != b.Month {
		return a.Month > b.Month
	}
	return a.Day > b.Day
}

// EventRecurrenceRepository persists recurrence rules and materialises their
// occurrences. Get* return ErrNotFound when absent.
type EventRecurrenceRepository interface {
	// Create inserts a new rule. An unknown restaurant_id (FK violation) maps to
	// ErrNotFound, same convention as EventRepository.Create.
	Create(ctx context.Context, r *EventRecurrence) error
	// GetByID returns a rule regardless of its active flag (staff resolve the
	// target and its restaurant before authorizing).
	GetByID(ctx context.Context, id uuid.UUID) (*EventRecurrence, error)
	// Update overwrites the mutable fields of an existing rule.
	Update(ctx context.Context, r *EventRecurrence) error
	// SetActive flips the active flag without touching the template. It never
	// removes occurrences that already exist.
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
	// ListByRestaurant returns a venue's rules for the cabinet, newest first
	// with id as a stable tie-breaker, paginated, plus the total count.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]EventRecurrence, int, error)
	// ListActive returns active rules with their venue's zone, keyset-paginated
	// by id (pass uuid.Nil for the first page). This is the generator's scan.
	ListActive(ctx context.Context, afterID uuid.UUID, limit int) ([]ActiveEventRecurrence, error)
	// InsertOccurrences materialises the given slots as `events` rows carrying
	// the rule's template, and returns how many rows were actually inserted.
	//
	// It MUST be idempotent and safe to run concurrently: a slot that already
	// has an event (in ANY status, however edited) and a slot carrying a
	// tombstone are both skipped, and the (recurrence_id, starts_at) unique
	// index decides the winner when two workers insert the same slot at once.
	InsertOccurrences(ctx context.Context, r *EventRecurrence, slots []time.Time) (int, error)
	// RecordSkip tombstones one slot so the generator never fills it again. It
	// is idempotent. Used when a generated occurrence is deleted outright or
	// moved to another time — the cases the unique index cannot cover, because
	// the row that occupied the slot is gone.
	RecordSkip(ctx context.Context, recurrenceID uuid.UUID, slot time.Time) error
}
