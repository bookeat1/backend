package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BlacklistEntry blocks a guest from booking. RestaurantID nil = a global
// (platform-wide) entry; set = scoped to that venue. At least one of UserID,
// PhoneNormalized, Email must be set. Phones are matched in E.164 and emails
// lower-cased — matching raw user input would never hit.
type BlacklistEntry struct {
	ID              uuid.UUID
	RestaurantID    *uuid.UUID
	UserID          *uuid.UUID
	PhoneNormalized *string
	Email           *string
	Reason          *string
	CreatedBy       *uuid.UUID
	IsActive        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// BlacklistQuery is the identity to check against the stop list. RestaurantID
// nil checks global entries only.
type BlacklistQuery struct {
	RestaurantID    *uuid.UUID
	UserID          *uuid.UUID
	PhoneNormalized string
	Email           string
}

// BookingBlacklistRepository persists the guest stop list.
type BookingBlacklistRepository interface {
	// Match returns the first active entry matching q, either global or scoped
	// to q.RestaurantID, or nil when the guest is not blacklisted.
	Match(ctx context.Context, q BlacklistQuery) (*BlacklistEntry, error)
	// ListByRestaurant returns active entries for the venue plus global ones.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]BlacklistEntry, error)
	// Create returns ErrAlreadyExists on a duplicate active entry.
	Create(ctx context.Context, e *BlacklistEntry) error
	Deactivate(ctx context.Context, id uuid.UUID) error
}

// RateLogAction is the throttled action recorded for anti-fraud purposes.
type RateLogAction string

const (
	RateLogCreate RateLogAction = "create"
	RateLogCancel RateLogAction = "cancel"
)

// Valid reports whether a is a known rate-log action.
func (a RateLogAction) Valid() bool {
	switch a {
	case RateLogCreate, RateLogCancel:
		return true
	}
	return false
}

// BookingRateLogEntry records a booking attempt for frequency-based anti-fraud.
type BookingRateLogEntry struct {
	ID              uuid.UUID
	UserID          *uuid.UUID
	PhoneNormalized *string
	Email           *string
	RestaurantID    *uuid.UUID
	Action          RateLogAction
	CreatedAt       time.Time
}

// BookingRateLogRepository persists the anti-fraud log.
type BookingRateLogRepository interface {
	Create(ctx context.Context, e *BookingRateLogEntry) error
	// CountSince counts entries for the phone (E.164) since the given time.
	CountSince(ctx context.Context, phoneNormalized string, action RateLogAction, since time.Time) (int, error)
}
