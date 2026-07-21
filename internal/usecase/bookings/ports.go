// Package bookings is the application logic for table reservations: policy
// resolution, availability, creation and the status machine.
//
// Layering notes:
//   - the package never imports another domain's concrete repository; the two
//     things it needs from the restaurants context (reading a venue and asking
//     "does this user manage it?") are declared here as minimal local ports and
//     bound to the concrete implementations in bootstrap/deps.go;
//   - Config mirrors bootstrap.BookingConfig so the usecase layer stays free of
//     any bootstrap import (same shape as auth.Config).
package bookings

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// restaurantReader is the minimal slice of the restaurant repository this
// package needs: load a venue to resolve its booking policy and schedule.
type restaurantReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
}

// policyWriter is the minimal slice of the restaurant repository needed to
// persist a venue's booking-policy overrides. Kept separate from
// restaurantReader so the read-only consumers of this package (availability,
// the worker) cannot accidentally gain write access to the catalog.
type policyWriter interface {
	UpdateBookingPolicy(ctx context.Context, restaurantID uuid.UUID, o domain.BookingPolicyOverride) error
}

// scheduleReader is the minimal slice of the restaurant "related" repository
// used by the availability engine (opening hours, bookable slots, tables).
type scheduleReader interface {
	ListWorkingHours(ctx context.Context, restaurantID uuid.UUID) ([]domain.WorkingHours, error)
	ListTimeSlots(ctx context.Context, restaurantID uuid.UUID) ([]domain.TimeSlot, error)
	ListTables(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantTable, error)
}

// managerChecker answers whether a user manages a restaurant. Bound to
// restaurants.ManagerUseCase in deps.
type managerChecker interface {
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
}

// Config is the global (level-1) booking policy plus anti-fraud thresholds. A
// restaurant may override the policy fields per venue; see resolvePolicy.
type Config struct {
	DefaultDuration       time.Duration
	DefaultBuffer         time.Duration
	DefaultLead           time.Duration
	DefaultHorizonDays    int
	DefaultCancelDeadline time.Duration
	DefaultConfirmSLA     time.Duration
	DefaultMaxGuests      int
	DefaultAutoConfirm    bool
	TimezoneFallback      string

	// RateWindow / RateLimit throttle booking attempts per normalized phone
	// (booking_rate_log). Zero values fall back to the constants below.
	RateWindow time.Duration
	RateLimit  int

	// SlotStep is the granularity used to generate bookable start times when a
	// venue defines opening hours but no explicit restaurant_time_slots rows.
	SlotStep time.Duration
}

// Package-level fallbacks, applied to any zero-valued Config field so a
// partially wired Config can never produce a nonsensical policy (e.g. a
// zero-length booking or an unlimited rate window).
const (
	defaultDuration       = 120 * time.Minute
	defaultLead           = 60 * time.Minute
	defaultHorizonDays    = 60
	defaultCancelDeadline = 180 * time.Minute
	defaultConfirmSLA     = 120 * time.Minute
	defaultMaxGuests      = 20
	defaultTimezone       = "Asia/Almaty"
	defaultRateWindow     = time.Hour
	defaultRateLimit      = 10
	defaultSlotStep       = 30 * time.Minute

	// maxCombinedTables caps how many tables may be joined for one booking
	// (spec §6.3: greedy pick, no more than three).
	maxCombinedTables = 3
)

// withDefaults fills zero-valued fields with the package fallbacks. Buffer is
// deliberately left alone: zero is a meaningful value (no cleanup gap).
func (c Config) withDefaults() Config {
	if c.DefaultDuration <= 0 {
		c.DefaultDuration = defaultDuration
	}
	if c.DefaultBuffer < 0 {
		c.DefaultBuffer = 0
	}
	if c.DefaultLead < 0 {
		c.DefaultLead = defaultLead
	}
	if c.DefaultHorizonDays <= 0 {
		c.DefaultHorizonDays = defaultHorizonDays
	}
	if c.DefaultCancelDeadline < 0 {
		c.DefaultCancelDeadline = defaultCancelDeadline
	}
	if c.DefaultConfirmSLA <= 0 {
		c.DefaultConfirmSLA = defaultConfirmSLA
	}
	if c.DefaultMaxGuests <= 0 {
		c.DefaultMaxGuests = defaultMaxGuests
	}
	if c.TimezoneFallback == "" {
		c.TimezoneFallback = defaultTimezone
	}
	if c.RateWindow <= 0 {
		c.RateWindow = defaultRateWindow
	}
	if c.RateLimit <= 0 {
		c.RateLimit = defaultRateLimit
	}
	if c.SlotStep <= 0 {
		c.SlotStep = defaultSlotStep
	}
	return c
}
