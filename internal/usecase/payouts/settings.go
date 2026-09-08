package payouts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// PayoutSettingsInput is a write of one venue's payout policy overrides. Both
// fields are three-state, exactly like the stored ones: nil CLEARS the override
// (the venue goes back to the platform default), a value SETS it. There is no
// "leave as is" — the write replaces the whole policy, so a caller can never
// half-update it and be surprised by the leftover half.
type PayoutSettingsInput struct {
	MinPayoutMinor *int64
	MaxHoldDays    *int
}

// PayoutSettingsView is what a caller gets back: the venue's own overrides
// (which may be empty) TOGETHER with the policy that actually applies after the
// platform defaults are layered in.
//
// Both are returned deliberately. "min_payout_minor: null" alone tells a venue
// nothing about when it will be paid; the effective numbers are the answer to
// the question it is really asking, and returning them from the same struct the
// daily runner resolves means the answer cannot drift from the behaviour.
type PayoutSettingsView struct {
	RestaurantID uuid.UUID
	Settings     domain.PayoutSettings
	Effective    domain.PayoutPolicy
}

// SetPayoutSettings writes one venue's payout policy overrides.
//
// SUPERADMIN ONLY, and this is the whole point of the endpoint's RBAC: these
// two numbers decide WHEN a venue's settled money leaves the platform and HOW
// MUCH must accumulate first. A venue able to write them could lower its own
// threshold to drain money faster than the platform's cash planning assumes, or
// raise it to park money across a reporting boundary. Same posture as
// generate/send (authorizeSuperadmin) rather than as the destination, which a
// venue legitimately owns because it only says WHERE its own money goes.
func (u *UseCase) SetPayoutSettings(ctx context.Context, actor Actor, restaurantID uuid.UUID, in PayoutSettingsInput) (*PayoutSettingsView, error) {
	if err := u.authorizeSuperadmin(actor); err != nil {
		return nil, err
	}
	if u.settings == nil {
		return nil, fmt.Errorf("%w: per-venue payout settings are not configured", domain.ErrNotFound)
	}
	if restaurantID == uuid.Nil {
		return nil, fmt.Errorf("%w: payout settings need a restaurant", domain.ErrValidation)
	}
	s := &domain.PayoutSettings{
		RestaurantID:   restaurantID,
		MinPayoutMinor: in.MinPayoutMinor,
		MaxHoldDays:    in.MaxHoldDays,
	}
	// The writer is recorded, not the request: a money knob whose history says
	// only "it changed" is not auditable.
	author := actor.UserID
	s.UpdatedBy = &author
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := u.settings.Upsert(ctx, s); err != nil {
		return nil, err
	}
	u.log.Info("venue payout settings updated",
		"restaurant_id", restaurantID, "by", actor.UserID,
		"min_payout_minor", minorOrDefault(s.MinPayoutMinor),
		"max_hold_days", daysOrDefault(s.MaxHoldDays))
	return u.viewOf(s, restaurantID), nil
}

// GetPayoutSettings returns a venue's overrides and its effective policy.
//
// READ is venue-scoped (owner/manager, restaurant.manage, tenant-scoped), with
// the usual superadmin bypass: a venue must be able to see when and above what
// amount it gets paid — that is its own money — it just must not be the one
// deciding it. A venue with no overrides is NOT an error: it gets an empty
// settings block and the platform's effective numbers.
func (u *UseCase) GetPayoutSettings(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*PayoutSettingsView, error) {
	if err := u.authorizeRestaurant(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	if u.settings == nil {
		// No repository wired: every venue follows the platform policy, which
		// is a truthful answer rather than a 404.
		return u.viewOf(nil, restaurantID), nil
	}
	s, err := u.settings.Get(ctx, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return u.viewOf(nil, restaurantID), nil
		}
		return nil, err
	}
	return u.viewOf(s, restaurantID), nil
}

// effectivePolicyFor resolves the policy that applies to one venue: its own
// overrides on top of the platform default. A nil settings pointer means "no
// overrides", which is the common case and must be cheap to express.
//
// This is THE resolution point. The daily runner and the read endpoint both go
// through it, so there is exactly one answer to "what policy applies here".
func (u *UseCase) effectivePolicyFor(s *domain.PayoutSettings) domain.PayoutPolicy {
	if s == nil {
		return u.cfg.PlatformPolicy
	}
	return s.Effective(u.cfg.PlatformPolicy)
}

func (u *UseCase) viewOf(s *domain.PayoutSettings, restaurantID uuid.UUID) *PayoutSettingsView {
	v := &PayoutSettingsView{
		RestaurantID: restaurantID,
		Settings:     domain.PayoutSettings{RestaurantID: restaurantID},
		Effective:    u.effectivePolicyFor(s),
	}
	if s != nil {
		v.Settings = *s
	}
	return v
}

// minorOrDefault / daysOrDefault render a three-state override for a log line:
// -1 stands for "not set, follows the platform". A log must not print a bare 0
// where the value was actually absent — 0 is a real, different policy.
func minorOrDefault(v *int64) int64 {
	if v == nil {
		return -1
	}
	return *v
}

func daysOrDefault(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}
