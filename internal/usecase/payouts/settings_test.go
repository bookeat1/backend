package payouts

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// RBAC: the platform sets a venue's payout policy, the venue only reads it
// ---------------------------------------------------------------------------

// TestSettings_VenueCannotWriteItsOwnPayoutPolicy is the money-safety RBAC test.
//
// Note the setup: the staff caller HOLDS restaurant.manage at this very venue
// (perms.allow = true) — the same permission that legitimately lets it set the
// payout DESTINATION. It must still be refused, because the threshold and the
// hold window decide WHEN and above what amount the platform disburses money,
// not merely where the venue's own money goes. A venue that could lower its
// threshold would drain settled money faster than cash planning assumes; one
// that could raise it could park money across a reporting boundary.
//
// MUTATION CHECK: swapping authorizeSuperadmin for authorizeRestaurant in
// SetPayoutSettings makes this test pass the write through and fail.
func TestSettings_VenueCannotWriteItsOwnPayoutPolicy(t *testing.T) {
	h := newHarness()
	h.perms.allow = true // the venue owner really does manage this restaurant
	rid := uuid.New()

	_, err := h.uc.SetPayoutSettings(context.Background(), staff(), rid,
		PayoutSettingsInput{MinPayoutMinor: ptrInt64(1)})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must never write its own payout policy, got %v", err)
	}
	if len(h.settings.m) != 0 {
		t.Fatal("nothing may be persisted by a forbidden write")
	}
}

// TestSettings_UnauthenticatedCallerIsRejected — no actor, no write.
func TestSettings_UnauthenticatedCallerIsRejected(t *testing.T) {
	h := newHarness()
	_, err := h.uc.SetPayoutSettings(context.Background(), Actor{}, uuid.New(), PayoutSettingsInput{})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	_, err = h.uc.GetPayoutSettings(context.Background(), Actor{}, uuid.New())
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on read, got %v", err)
	}
}

// TestSettings_SuperadminWritesAndTheVenueReadsIt: the intended flow end to end,
// including the audit trail of who set the policy.
func TestSettings_SuperadminWritesAndTheVenueReadsIt(t *testing.T) {
	h := newHarness()
	rid := uuid.New()
	admin := superadmin()

	written, err := h.uc.SetPayoutSettings(context.Background(), admin, rid,
		PayoutSettingsInput{MinPayoutMinor: ptrInt64(200_000), MaxHoldDays: ptrInt(3)})
	if err != nil {
		t.Fatalf("superadmin write: %v", err)
	}
	if written.Effective.MinPayoutMinor != 200_000 || written.Effective.MaxHoldDays != 3 {
		t.Fatalf("the write must report the policy now in force, got %+v", written.Effective)
	}
	stored := h.settings.m[rid]
	if stored.UpdatedBy == nil || *stored.UpdatedBy != admin.UserID {
		t.Fatal("the superadmin who changed a money knob must be recorded")
	}

	// The venue reads its own policy — allowed, and it sees the same numbers.
	h.perms.allow = true
	got, err := h.uc.GetPayoutSettings(context.Background(), staff(), rid)
	if err != nil {
		t.Fatalf("venue read: %v", err)
	}
	if got.Effective.MinPayoutMinor != 200_000 || got.Effective.MaxHoldDays != 3 {
		t.Fatalf("a venue must see the policy it is actually paid by, got %+v", got.Effective)
	}
	if h.perms.gotPerm != domain.PermRestaurantManage {
		t.Fatalf("the read must be gated by restaurant.manage, got %q", h.perms.gotPerm)
	}
	if h.perms.gotRest != rid {
		t.Fatalf("the permission must be checked AT the requested restaurant, got %s", h.perms.gotRest)
	}
}

// TestSettings_ForeignVenueCannotReadAnotherVenuesPolicy: the id in the path is
// only ever the key the permission is checked against.
func TestSettings_ForeignVenueCannotReadAnotherVenuesPolicy(t *testing.T) {
	h := newHarness()
	h.perms.allow = false
	_, err := h.uc.GetPayoutSettings(context.Background(), staff(), uuid.New())
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for a venue with no rights there, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resolution and validation
// ---------------------------------------------------------------------------

// TestSettings_ReadWithoutOverridesReportsThePlatformPolicy: a venue that has
// never been configured is not a 404 — it follows the platform, and must be
// told which numbers those are.
func TestSettings_ReadWithoutOverridesReportsThePlatformPolicy(t *testing.T) {
	h := newHarnessWithConfig(Config{
		PlatformPolicy: domain.PayoutPolicy{MinPayoutMinor: 1_000_000, MaxHoldDays: 7},
	})
	h.perms.allow = true

	got, err := h.uc.GetPayoutSettings(context.Background(), staff(), uuid.New())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Settings.MinPayoutMinor != nil || got.Settings.MaxHoldDays != nil {
		t.Fatalf("a venue with no overrides must report none, got %+v", got.Settings)
	}
	if got.Effective.MinPayoutMinor != 1_000_000 || got.Effective.MaxHoldDays != 7 {
		t.Fatalf("expected the platform policy as effective, got %+v", got.Effective)
	}
}

// TestSettings_ClearingAnOverrideRestoresThePlatformDefault: sending null is a
// real edit, not a no-op.
func TestSettings_ClearingAnOverrideRestoresThePlatformDefault(t *testing.T) {
	h := newHarnessWithConfig(Config{
		PlatformPolicy: domain.PayoutPolicy{MinPayoutMinor: 1_000_000, MaxHoldDays: 7},
	})
	rid := uuid.New()
	if _, err := h.uc.SetPayoutSettings(context.Background(), superadmin(), rid,
		PayoutSettingsInput{MinPayoutMinor: ptrInt64(50_000)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	cleared, err := h.uc.SetPayoutSettings(context.Background(), superadmin(), rid, PayoutSettingsInput{})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.Settings.MinPayoutMinor != nil {
		t.Fatalf("the override must be cleared, got %d", *cleared.Settings.MinPayoutMinor)
	}
	if cleared.Effective.MinPayoutMinor != 1_000_000 {
		t.Fatalf("clearing must restore the platform default, got %d", cleared.Effective.MinPayoutMinor)
	}
}

// TestSettings_ZeroIsAnExplicitPolicyNotAnUnsetOne: 0 must survive the round
// trip as 0, otherwise "pay any positive balance" and "never force" become
// unexpressible.
func TestSettings_ZeroIsAnExplicitPolicyNotAnUnsetOne(t *testing.T) {
	h := newHarnessWithConfig(Config{
		PlatformPolicy: domain.PayoutPolicy{MinPayoutMinor: 1_000_000, MaxHoldDays: 7},
	})
	rid := uuid.New()
	got, err := h.uc.SetPayoutSettings(context.Background(), superadmin(), rid,
		PayoutSettingsInput{MinPayoutMinor: ptrInt64(0), MaxHoldDays: ptrInt(0)})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.Effective.MinPayoutMinor != 0 || got.Effective.MaxHoldDays != 0 {
		t.Fatalf("an explicit zero must win over the platform default, got %+v", got.Effective)
	}
}

// TestSettings_RejectsOutOfRangeValues: the guard rail against a typo that
// would strand a venue's money behind a threshold it can never reach.
func TestSettings_RejectsOutOfRangeValues(t *testing.T) {
	h := newHarness()
	cases := map[string]PayoutSettingsInput{
		"negative threshold":  {MinPayoutMinor: ptrInt64(-1)},
		"absurd threshold":    {MinPayoutMinor: ptrInt64(100_000_001)},
		"negative hold days":  {MaxHoldDays: ptrInt(-1)},
		"hold days > a year":  {MaxHoldDays: ptrInt(366)},
		"absurd both at once": {MinPayoutMinor: ptrInt64(1 << 40), MaxHoldDays: ptrInt(5000)},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			rid := uuid.New()
			_, err := h.uc.SetPayoutSettings(context.Background(), superadmin(), rid, in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
			if _, stored := h.settings.m[rid]; stored {
				t.Fatal("an invalid policy must not be persisted")
			}
		})
	}
}

// TestPayoutSettings_EffectiveIsPerField: the layering rule itself, isolated.
func TestPayoutSettings_EffectiveIsPerField(t *testing.T) {
	def := domain.PayoutPolicy{MinPayoutMinor: 1_000_000, MaxHoldDays: 7}

	none := domain.PayoutSettings{}.Effective(def)
	if none != def {
		t.Fatalf("no overrides must leave the default untouched, got %+v", none)
	}
	onlyMin := domain.PayoutSettings{MinPayoutMinor: ptrInt64(5)}.Effective(def)
	if onlyMin.MinPayoutMinor != 5 || onlyMin.MaxHoldDays != 7 {
		t.Fatalf("overriding one field must not disturb the other, got %+v", onlyMin)
	}
	onlyHold := domain.PayoutSettings{MaxHoldDays: ptrInt(2)}.Effective(def)
	if onlyHold.MinPayoutMinor != 1_000_000 || onlyHold.MaxHoldDays != 2 {
		t.Fatalf("overriding one field must not disturb the other, got %+v", onlyHold)
	}
}
