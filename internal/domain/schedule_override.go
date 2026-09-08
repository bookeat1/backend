package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ScheduleOverride is a special-day exception to a restaurant's regular weekly
// working hours (WorkingHours, keyed by day_of_week): a holiday closure or a
// one-off change of hours on a specific calendar date. Regular hours live in
// the existing working_hours table; this table holds ONLY the exceptions, so a
// date with no override row simply follows the weekly schedule.
//
// When IsClosed is true the venue is shut that whole day and OpenTime/CloseTime
// are ignored (and stored NULL). When IsClosed is false both times must be set
// — that is a "open, but with different hours than usual" day. Times are
// "HH:MM" strings in the venue's own timezone, matching WorkingHours.OpenTime/
// CloseTime, not a wall-clock instant.
type ScheduleOverride struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Date         time.Time // the calendar day this override applies to (date only)
	IsClosed     bool
	OpenTime     *string
	CloseTime    *string
	Note         *string
	// BookingPaymentRequired marks this special day as PAID: the guest must
	// place a prepayment (deposit) to book on this date. Bookings are FREE by
	// default (migration 0036) — false here means the day follows the
	// restaurant's ordinary payment settings, true means a deposit of
	// DepositAmountMinor is required to book that day.
	BookingPaymentRequired bool
	// DepositAmountMinor is the required prepayment for a paid special day, in
	// int64 MINOR units (never a float). NULL/nil on a free day; a DB CHECK
	// guarantees it is present and > 0 whenever BookingPaymentRequired is true.
	DepositAmountMinor *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ScheduleOverrideRepository persists a restaurant's special-day overrides.
// Every method is scoped by restaurantID so a caller can never read or mutate
// another venue's schedule through this port.
type ScheduleOverrideRepository interface {
	// ListByRestaurant returns the venue's overrides ordered by date ascending.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]ScheduleOverride, error)
	// GetForBookingInstant returns the override whose override_date equals the
	// CALENDAR DATE of `at` in the venue's own timezone (restaurants.timezone,
	// falling back to fallbackTZ when the venue has no timezone set or it is not
	// a recognized IANA name). It exists specifically for the prepayment
	// decision on booking/payment creation: a booking is an instant, but a
	// special day is a local calendar date, so the two must be compared in the
	// venue's zone, not UTC. Returns ErrNotFound when the venue has no override
	// for that date (a normal day).
	GetForBookingInstant(ctx context.Context, restaurantID uuid.UUID, at time.Time, fallbackTZ string) (*ScheduleOverride, error)
	// Upsert inserts or replaces the override for (restaurant_id, date). The
	// unique key on that pair makes "set the override for this day" idempotent
	// — a second call for the same day updates in place rather than duplicating.
	Upsert(ctx context.Context, o *ScheduleOverride) error
	// Delete removes the override for (restaurant_id, date), reverting that day
	// to the regular weekly schedule. Returns ErrNotFound when no override
	// exists for that day.
	Delete(ctx context.Context, restaurantID uuid.UUID, date time.Time) error
}

// RestaurantGuest is a read-model row for the venue's guest list: one distinct
// guest (identified by their normalized phone number, the stable identity a
// booking always carries even for a guest with no account) aggregated across
// all their bookings at this restaurant. It is derived entirely from the
// bookings table — there is no guests table — so it is strictly read-only.
type RestaurantGuest struct {
	UserID          *uuid.UUID // set when at least one of the guest's bookings had an account
	Name            string     // most recent name the guest booked under
	Phone           string     // most recent phone as typed
	PhoneNormalized string     // E.164, the grouping key
	Email           string     // most recent email
	BookingsCount   int
	VisitsCount     int       // bookings that reached arrived/completed
	FirstBookingAt  time.Time // earliest booking created_at
	LastBookingAt   time.Time // most recent booking created_at
}
