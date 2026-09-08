package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PayoutPolicy is the EFFECTIVE payout policy applied to one venue: the two
// numbers the daily pass actually decides with, after a venue's own override
// has been layered on top of the platform default.
//
// It exists as its own type so "what the platform configured" and "what applies
// to this venue" can never be confused for each other — the daily runner and
// the venue-facing read endpoint resolve the same struct through the same
// function (PayoutSettings.Effective), which is what keeps the number a venue
// is shown identical to the number it is paid by.
type PayoutPolicy struct {
	// MinPayoutMinor is the threshold below which a venue's settled money rolls
	// into the next day instead of being paid (the acquirer's per-payout floor
	// makes a small payout disproportionately expensive).
	MinPayoutMinor int64
	// MaxHoldDays caps how long that roll-over may continue: once the venue's
	// OLDEST unpaid money has been held this many whole venue-local days, the
	// next pass pays out regardless of the threshold. Without it a venue that
	// never reaches MinPayoutMinor would have its money held forever, which is
	// not ours to hold. 0 disables the cap (roll over indefinitely).
	MaxHoldDays int
}

// PayoutSettings is ONE venue's override of the platform payout policy. Both
// knobs are pointers with a deliberate three-state meaning:
//
//	nil        — this venue has no opinion; the platform default applies
//	non-nil    — this venue's own value, which WINS over the platform default
//
// A pointer rather than a zero-value sentinel because 0 is a legitimate value
// for both fields (a zero threshold means "pay any positive balance", a zero
// hold cap means "never force"), so "unset" and "explicitly zero" must stay
// distinguishable — collapsing them is how a venue silently gets a policy
// nobody chose.
//
// WHO MAY WRITE THIS: the platform (superadmin) only. A venue may read its own
// settings but must not set them — a venue that could lower its own threshold
// would drain settled money faster than the platform's own cash planning
// assumes, and one that could raise it could park money to game a reporting
// period. Enforced in usecase/payouts, not here.
type PayoutSettings struct {
	RestaurantID uuid.UUID
	// MinPayoutMinor overrides PayoutPolicy.MinPayoutMinor for this venue.
	MinPayoutMinor *int64
	// MaxHoldDays overrides PayoutPolicy.MaxHoldDays for this venue.
	MaxHoldDays *int
	// UpdatedBy is the superadmin who last wrote these settings. Kept because
	// this row changes when a venue is paid and how much it must accumulate
	// first — a money knob with no trace of who turned it is not auditable.
	UpdatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

const (
	// maxVenueMinPayoutMinor — 1 000 000 ₸ (100 000 000 tiyn), the largest
	// per-venue threshold that can be configured.
	//
	// This is a GUARD RAIL, not a policy: a superadmin typing one zero too many
	// would otherwise strand a venue's money behind a threshold it can never
	// reach. MaxHoldDays is the real safety net, but a cap that makes the typo
	// impossible in the first place is cheaper than a support ticket about
	// money that never arrived.
	maxVenueMinPayoutMinor int64 = 100_000_000
	// maxVenueHoldDays — a year. Beyond this the "hold" stops being a batching
	// optimisation and becomes us sitting on someone else's money.
	maxVenueHoldDays = 365
)

// Validate checks a venue's overrides before they are stored. Both fields are
// optional; only a PRESENT one is checked.
func (s PayoutSettings) Validate() error {
	if s.RestaurantID == uuid.Nil {
		return fmt.Errorf("%w: payout settings need a restaurant", ErrValidation)
	}
	if s.MinPayoutMinor != nil {
		if *s.MinPayoutMinor < 0 {
			return fmt.Errorf("%w: min payout amount cannot be negative", ErrValidation)
		}
		if *s.MinPayoutMinor > maxVenueMinPayoutMinor {
			return fmt.Errorf("%w: min payout amount above the configurable maximum (%d minor units)",
				ErrValidation, maxVenueMinPayoutMinor)
		}
	}
	if s.MaxHoldDays != nil {
		if *s.MaxHoldDays < 0 {
			return fmt.Errorf("%w: max hold days cannot be negative", ErrValidation)
		}
		if *s.MaxHoldDays > maxVenueHoldDays {
			return fmt.Errorf("%w: max hold days above the configurable maximum (%d)",
				ErrValidation, maxVenueHoldDays)
		}
	}
	return nil
}

// Effective layers this venue's overrides on top of the platform default and
// returns the policy that actually applies. A nil field falls through to def —
// per field, not per row, so a venue may override its threshold while still
// following the platform's hold cap.
func (s PayoutSettings) Effective(def PayoutPolicy) PayoutPolicy {
	out := def
	if s.MinPayoutMinor != nil {
		out.MinPayoutMinor = *s.MinPayoutMinor
	}
	if s.MaxHoldDays != nil {
		out.MaxHoldDays = *s.MaxHoldDays
	}
	return out
}

// PayoutSettingsRepository persists per-venue payout overrides.
//
// A venue with no row at all is the normal case (the vast majority follow the
// platform default), so "absent" must be cheap and unremarkable: Get returns
// ErrNotFound and ForRestaurants simply omits the id.
type PayoutSettingsRepository interface {
	// Get returns one venue's overrides, or ErrNotFound when it has none.
	Get(ctx context.Context, restaurantID uuid.UUID) (*PayoutSettings, error)
	// Upsert stores a venue's overrides in place (one row per restaurant).
	Upsert(ctx context.Context, s *PayoutSettings) error
	// ForRestaurants resolves overrides for many venues in ONE query — the
	// daily pass touches every owed venue per tick and must not do N reads.
	// Venues without a row are absent from the map.
	ForRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]PayoutSettings, error)
}
