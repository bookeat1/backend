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
