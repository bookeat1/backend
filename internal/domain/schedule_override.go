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
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ScheduleOverrideRepository persists a restaurant's special-day overrides.
// Every method is scoped by restaurantID so a caller can never read or mutate
// another venue's schedule through this port.
type ScheduleOverrideRepository interface {
	// ListByRestaurant returns the venue's overrides ordered by date ascending.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]ScheduleOverride, error)
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
