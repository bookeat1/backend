package bookings

import (
	"context"
	"errors"
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
	schedule    scheduleReader
	capacity    domain.BookingCapacityRepository
	bookings    bookingLister
	tx          domain.TxManager
	cfg         Config
}

// NewPolicyUseCase constructs the venue booking-policy usecase.
//
// schedule, capacity, bookings and tx are consulted ONLY when the capacity mode
// or the declared capacity is being changed — every one of them may be nil, in
// which case such a change is refused instead of being performed blind. Plain
// policy edits (duration, buffer, auto-confirm…) never touch them.
func NewPolicyUseCase(
	restaurants restaurantReader,
	policies policyWriter,
	managers managerChecker,
	schedule scheduleReader,
	capacity domain.BookingCapacityRepository,
	bookings bookingLister,
	tx domain.TxManager,
	cfg Config,
) PolicyUseCase {
	return &policyUseCase{
		restaurants: restaurants, policies: policies, managers: managers,
		schedule: schedule, capacity: capacity, bookings: bookings, tx: tx, cfg: cfg,
	}
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
	if in.BookingCapacityMode == nil && in.BookingCapacitySeats == nil {
		// A single UPDATE is atomic on its own; the read that follows only
		// builds the response, so no transaction is needed here. Existing
		// bookings are deliberately left untouched — a policy change applies to
		// future ones.
		if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
			return nil, err
		}
		return u.view(ctx, restaurantID)
	}
	if err := u.applyCapacityChange(ctx, restaurantID, in); err != nil {
		return nil, err
	}
	return u.view(ctx, restaurantID)
}

// applyCapacityChange writes a capacity-mode / capacity-size change together
// with everything that has to stay true afterwards. It is the one policy edit
// that is NOT a lone column write, because the two modes account for occupancy
// in two different places and the venue's existing bookings have to survive the
// move between them.
//
// What happens to bookings, explicitly:
//
//   - tables → seats: every FUTURE booking that still holds a seat is given
//     capacity holds computed from its own time and party size, inside the same
//     transaction as the mode switch. Without that backfill those bookings
//     would be invisible to the new engine and the venue would be sold their
//     seats a second time. If they do not fit into the newly declared capacity,
//     the whole switch is refused — the venue is told which moment is over
//     capacity instead of quietly losing guests.
//   - seats → tables: the existing table-less bookings stay valid, keep their
//     holds and stay visible in every listing, but they hold no specific table
//     (they never had one — staff seated them by hand). Switching is refused
//     unless the venue actually has active tables, otherwise the switch would
//     recreate the very "every slot says capacity" blocker this feature fixes.
//   - raising / lowering the declared capacity: existing bookings are never
//     touched. Lowering below what is already sold for a future moment is
//     refused, because the alternative is a booking the venue has confirmed and
//     can no longer honour.
func (u *policyUseCase) applyCapacityChange(ctx context.Context, restaurantID uuid.UUID, in domain.BookingPolicyOverride) error {
	if u.capacity == nil || u.bookings == nil || u.tx == nil || u.schedule == nil {
		return fmt.Errorf("%w: capacity mode is not configured on this deployment", domain.ErrValidation)
	}
	agg, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return err
	}
	current := resolvePolicy(agg.Restaurant, u.cfg)

	// The target state is the patch applied on top of what is stored today: a
	// venue may set the capacity now and flip the mode later, or vice versa.
	target := current
	if in.BookingCapacityMode != nil {
		target.CapacityMode = *in.BookingCapacityMode
	}
	if in.BookingCapacitySeats != nil {
		target.CapacitySeats = *in.BookingCapacitySeats
	}
	if target.CapacityMode == domain.CapacityModeSeats && target.CapacitySeats <= 0 {
		return fmt.Errorf("%w: booking_capacity_seats is required to book by total capacity",
			domain.ErrValidation)
	}

	if target.CapacityMode == domain.CapacityModeTables {
		tables, err := u.schedule.ListTables(ctx, restaurantID)
		if err != nil {
			return err
		}
		active := 0
		for _, t := range tables {
			if t.IsActive && t.Capacity > 0 {
				active++
			}
		}
		if active == 0 {
			return fmt.Errorf("%w: this restaurant has no active tables, so booking by tables would make every slot unbookable",
				domain.ErrValidation)
		}
		return u.policies.UpdateBookingPolicy(ctx, restaurantID, in)
	}

	// Lowering the capacity: refuse while the venue has more guests already
	// booked for a future moment than the new number allows. Checked before the
	// write for a readable error; the bucket CHECK would refuse it anyway.
	if peak, err := u.capacity.PeakTaken(ctx, restaurantID, time.Now()); err != nil {
		return err
	} else if peak != nil && peak.SeatsTaken > target.CapacitySeats {
		return fmt.Errorf("%w: %d guests are already booked for %s; capacity cannot be set below that",
			domain.ErrValidation, peak.SeatsTaken, peak.BucketStart.UTC().Format(time.RFC3339))
	}

	return u.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Same per-venue lock a booking create takes, and taken FIRST: the
		// policy write and the holds rebuilt from it must not interleave with a
		// create that read the previous policy.
		if err := u.capacity.LockVenue(ctx, restaurantID); err != nil {
			return err
		}
		if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
			return err
		}
		return u.rebuildHolds(ctx, restaurantID, target)
	})
}

// rebuildHolds (re)writes the capacity holds of every future booking that still
// holds a seat, so the bucket counters describe the venue's real load under the
// declared capacity. ReplaceForBooking rather than Create: a venue that has
// switched modes back and forth must not trip over the holds of the previous
// round.
func (u *policyUseCase) rebuildHolds(ctx context.Context, restaurantID uuid.UUID, policy domain.BookingPolicy) error {
	// A booking that STARTED before now but has not finished still occupies the
	// room, so the backfill has to reach backwards, not just forward. Listing
	// from now (the filter means starts_at >= from) silently dropped every
	// in-progress visit: switch a 40-seat venue to seats mode at 20:30 while a
	// party of 30 is seated until 22:00, and the ledger reads empty for those
	// buckets — the next party of 30 is accepted and the room is oversold by
	// exactly the guests already sitting in it. Reaching back one full
	// occupancy window (duration + both buffers, plus one bucket for the
	// outward rounding) covers any booking whose window still touches the
	// future; anything older cannot be holding a seat now.
	lookback := policy.Duration + 2*policy.Buffer + domain.CapacityBucket
	from := time.Now().Add(-lookback)
	for page := 1; ; page++ {
		list, total, err := u.bookings.List(ctx, domain.BookingFilter{
			RestaurantID: &restaurantID,
			Statuses:     domain.StatusesHoldingTable(),
			From:         &from,
			Page:         page,
			PerPage:      capacityBackfillPage,
		})
		if err != nil {
			return err
		}
		for i := range list {
			b := list[i]
			if b.Guests > policy.CapacitySeats {
				return fmt.Errorf("%w: booking of %d guests on %s does not fit a capacity of %d",
					domain.ErrValidation, b.Guests, b.StartsAt.UTC().Format(time.RFC3339), policy.CapacitySeats)
			}
			holds := buildCapacityHolds(&b, policy, time.Now())
			if err := u.capacity.ReplaceForBooking(ctx, b.ID, holds); err != nil {
				if errors.Is(err, domain.ErrAlreadyExists) {
					return fmt.Errorf("%w: the bookings already accepted for %s exceed a capacity of %d",
						domain.ErrValidation, b.StartsAt.UTC().Format(time.RFC3339), policy.CapacitySeats)
				}
				return err
			}
		}
		if len(list) == 0 || page*capacityBackfillPage >= total {
			return nil
		}
	}
}

// capacityBackfillPage is the page size of the mode-switch backfill. It matches
// the repository's own cap (100), so asking for more would silently return
// fewer rows than the loop expects.
const capacityBackfillPage = 100

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
	if m := o.BookingCapacityMode; m != nil && !m.Valid() {
		return fmt.Errorf("%w: booking_capacity_mode must be %q or %q",
			domain.ErrValidation, domain.CapacityModeTables, domain.CapacityModeSeats)
	}
	// The lower bound is 1, not 0: "we seat nobody" is not a configuration, it
	// is a venue that should be deactivated. The upper bound is deliberately
	// generous — see maxCapacitySeats — because its job is to catch a typo, not
	// to second-guess how big a banquet hall may be.
	if s := o.BookingCapacitySeats; s != nil && (*s < 1 || *s > maxCapacitySeats) {
		return fmt.Errorf("%w: booking_capacity_seats must be between 1 and %d",
			domain.ErrValidation, maxCapacitySeats)
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
