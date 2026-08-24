package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// VenueDashboard is what ONE restaurant sees about itself. It is deliberately
// separate from the platform dashboard: the platform view answers "how is
// BookEat doing" for the owner, this one answers "how is my venue doing" for
// the people who run it, and the two are read by different roles with
// different rights.
type VenueDashboard struct {
	From time.Time
	To   time.Time
	// ByStatus carries every status, including the ones with zero bookings —
	// an absent "cancelled" row and a zero one mean different things on a
	// screen, and the venue needs to see the zero.
	ByStatus []BookingStatusCount
	Total    int64
	// CancelledShare is cancelled+no_show over Total, as a percentage rounded
	// to one decimal. Zero when there were no bookings at all: a venue with no
	// bookings has no cancellation problem, and 0/0 must not read as 100%.
	CancelledShare float64
	// AvgPartySize is the mean guests per booking over the period, rounded to
	// one decimal. Zero when there were no bookings.
	AvgPartySize float64
	// CancelReasons is why guests cancelled, most common first. Bookings
	// cancelled without a reason code are grouped under an empty Reason so the
	// venue can see how much of its cancellation picture is unexplained.
	CancelReasons []CancelReasonCount
	// PreorderBookings is how many bookings in the period carried a pre-order,
	// and PreorderTotalMinor their total value in integer minor units.
	PreorderBookings   int
	PreorderTotalMinor int64
}

// CancelReasonCount is one row of the cancellation breakdown.
type CancelReasonCount struct {
	Reason string
	Count  int
}

// VenueLoadSlot is occupancy for one hour of one weekday, aggregated over the
// period. Weekday follows time.Weekday (0 = Sunday), the same convention as
// restaurant_working_hours.day_of_week.
type VenueLoadSlot struct {
	Weekday  int
	Hour     int
	Bookings int
	Guests   int
}

// VenueDashboardRepository is the venue-scoped read model. Every method takes
// restaurantID first and MUST filter by it: this is the tenant boundary of the
// whole feature, and a missing WHERE here would show one venue another's
// numbers.
type VenueDashboardRepository interface {
	Summary(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) (VenueDashboard, error)
	Load(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]VenueLoadSlot, error)
}

// VenueToday is what the panel shows before any numbers: the work waiting to be
// done right now.
//
// It is deliberately not a slice of the dashboard above. The dashboard answers
// "how did we do", this answers "what do I do next", and the two are read by
// different people at different moments — a hostess mid-shift and an owner on
// Sunday evening.
type VenueToday struct {
	// Awaiting are requests the venue has not answered yet, oldest FIRST: the
	// guest who has been waiting longest is the one most likely to give up.
	// Includes future dates, not just today — a request for Saturday needs an
	// answer today.
	Awaiting []VenueTodayBooking
	// Today is every live booking of the venue's current local day, in time
	// order. Cancelled ones are left out: they are not work.
	Today []VenueTodayBooking
	// Guests is how many people are expected today across those bookings. It
	// counts the venue's WHOLE local day, not just the rows that fit under
	// TodayLimit — a truncated list must not turn into a wrong headcount.
	Guests int
	// AwaitingTotal and TodayTotal are how many rows exist before the limits
	// are applied, so the panel can say "20 of 34" instead of pretending the
	// truncated list is everything.
	AwaitingTotal int
	TodayTotal    int
}

// VenueTodayBooking is the slice of a booking this screen needs: when, who, how
// many, and — for a request still waiting — how long it has been ignored.
//
// It is deliberately not a full Booking. The panel renders a row, not a booking
// card, and reading thirty columns per row to display four of them is the kind
// of thing that is cheap once and expensive at every venue at 19:00.
type VenueTodayBooking struct {
	ID       uuid.UUID
	StartsAt time.Time
	// Name and Phone are the guest as the venue must reach them: the phone is
	// the raw one the guest typed, the same value the booking screens show.
	Name   string
	Phone  string
	Guests int
	Status BookingStatus
	// CreatedAt is when the request arrived — the clock WaitingMinutes runs on.
	CreatedAt time.Time
	// WaitingMinutes is whole minutes between CreatedAt and the moment the
	// screen was rendered, computed server-side so every device agrees. It is
	// meaningful for Awaiting rows; for an answered booking it is simply the
	// age of the record. Never negative: a created_at in the future (clock skew
	// on an import) reads as 0, not as a negative wait.
	WaitingMinutes int
	// Preorder is what the guest ordered ahead, if anything.
	//
	// It lives on THIS row, and not only in the bookings list, because the
	// hostess decides whether to accept a request from this screen: four
	// pre-ordered dishes are an argument for saying yes, and the kitchen needs
	// them before the guest arrives, not after someone opens another tab.
	Preorder []BookingItem
}

// VenueTodayRepository reads the panel's operational view.
//
// now is passed in rather than read from the clock inside the query: the
// venue's "today" and every waiting time must be measured against ONE instant,
// and a test must be able to name it.
type VenueTodayRepository interface {
	Today(ctx context.Context, restaurantID uuid.UUID, now time.Time, awaitingLimit, todayLimit int) (VenueToday, error)
}
