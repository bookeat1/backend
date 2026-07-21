package bookings

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// PolicyUseCase reads and edits one venue's booking-policy overrides (spec §4.2
// level 2). Without it the columns added in wave 3 are write-only via raw SQL:
// a venue could be read as "auto_confirm = false" but never actually set that
// way through the product.
//
// Authorization matches the rest of the venue cabinet: the restaurant's own
// manager or an admin; a manager of another venue gets ErrForbidden.
type PolicyUseCase interface {
	// Get returns the stored overrides plus the effective policy they resolve to.
	Get(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*PolicyView, error)
	// Update applies a PATCH of the overrides (nil field = leave as-is) and
	// returns the resulting effective policy.
	Update(ctx context.Context, actor Actor, restaurantID uuid.UUID, in domain.BookingPolicyOverride) (*PolicyView, error)
}

// PolicyView pairs what the venue actually stores (nil = "inherit the global
// default") with the policy those overrides resolve to. The client needs both:
// the overrides to render the form, the effective values to show what the
// blank fields currently mean.
type PolicyView struct {
	Override  domain.BookingPolicyOverride
	Effective domain.BookingPolicy
}

// Bounds for the editable policy fields. They are enforced here rather than as
// DB CHECKs because the columns predate this endpoint and legacy rows may hold
// anything; resolvePolicy already ignores nonsense on read, this stops it from
// being written in the first place.
const (
	minPolicyDurationMinutes = 15
	maxPolicyDurationMinutes = 600
	maxPolicyBufferMinutes   = 240
	maxPolicyLeadMinutes     = 30 * 24 * 60
	minPolicyHorizonDays     = 1
	maxPolicyHorizonDays     = 365
	maxPolicyCancelMinutes   = 7 * 24 * 60
	minPolicySLAMinutes      = 1
	maxPolicySLAMinutes      = 24 * 60
	minPolicyMaxGuests       = 1
	maxPolicyMaxGuests       = 100
)

type policyUseCase struct {
	restaurants restaurantReader
	policies    policyWriter
	managers    managerChecker
	cfg         Config
}

// NewPolicyUseCase constructs the venue booking-policy usecase.
func NewPolicyUseCase(restaurants restaurantReader, policies policyWriter, managers managerChecker, cfg Config) PolicyUseCase {
	return &policyUseCase{restaurants: restaurants, policies: policies, managers: managers, cfg: cfg}
}

func (u *policyUseCase) Get(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*PolicyView, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	return u.view(ctx, restaurantID)
}

func (u *policyUseCase) Update(ctx context.Context, actor Actor, restaurantID uuid.UUID, in domain.BookingPolicyOverride) (*PolicyView, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	in = normalizePolicyOverride(in)
	if err := validatePolicyOverride(in); err != nil {
		return nil, err
	}
	// A single UPDATE is atomic on its own; the read that follows only builds
	// the response, so no transaction is needed here. Existing bookings are
	// deliberately left untouched — a policy change applies to future ones.
	if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
		return nil, err
	}
	return u.view(ctx, restaurantID)
}

func (u *policyUseCase) view(ctx context.Context, restaurantID uuid.UUID) (*PolicyView, error) {
	agg, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	return &PolicyView{
		Override:  agg.BookingPolicy,
		Effective: resolvePolicy(agg.Restaurant, u.cfg),
	}, nil
}

// validatePolicyOverride checks the fields the caller actually provided. Omitted
// (nil) fields are not validated: they are not being written.
func validatePolicyOverride(o domain.BookingPolicyOverride) error {
	if o.Timezone != nil {
		tz := strings.TrimSpace(*o.Timezone)
		if tz == "" {
			return fmt.Errorf("%w: timezone must not be empty", domain.ErrValidation)
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("%w: unknown timezone %q", domain.ErrValidation, tz)
		}
	}
	checks := []struct {
		name     string
		val      *int
		min, max int
	}{
		{"booking_duration_minutes", o.BookingDurationMinutes, minPolicyDurationMinutes, maxPolicyDurationMinutes},
		{"booking_buffer_minutes", o.BookingBufferMinutes, 0, maxPolicyBufferMinutes},
		{"booking_lead_minutes", o.BookingLeadMinutes, 0, maxPolicyLeadMinutes},
		{"booking_horizon_days", o.BookingHorizonDays, minPolicyHorizonDays, maxPolicyHorizonDays},
		{"cancel_deadline_minutes", o.CancelDeadlineMinutes, 0, maxPolicyCancelMinutes},
		{"confirm_sla_minutes", o.ConfirmSLAMinutes, minPolicySLAMinutes, maxPolicySLAMinutes},
		{"max_guests_per_booking", o.MaxGuestsPerBooking, minPolicyMaxGuests, maxPolicyMaxGuests},
	}
	for _, c := range checks {
		if c.val == nil {
			continue
		}
		if *c.val < c.min || *c.val > c.max {
			return fmt.Errorf("%w: %s must be between %d and %d", domain.ErrValidation, c.name, c.min, c.max)
		}
	}
	return nil
}

// normalizePolicyOverride trims the timezone so a stray space can't produce a
// value that validates but fails time.LoadLocation on read.
func normalizePolicyOverride(o domain.BookingPolicyOverride) domain.BookingPolicyOverride {
	if o.Timezone != nil {
		tz := strings.TrimSpace(*o.Timezone)
		o.Timezone = &tz
	}
	return o
}
