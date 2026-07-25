package payouts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// This file covers the PER-VENUE payout policy (migration 0053): a venue's own
// threshold and max-hold window, with the platform env values as the fallback,
// and the payout the max-hold rule forces when a venue never accumulates enough.
//
// The money question behind every test here: a venue whose daily turnover never
// reaches the platform threshold must still be paid eventually, exactly once,
// and must be able to see WHY it was paid.

// ---------------------------------------------------------------------------
// 1. Override vs fallback
// ---------------------------------------------------------------------------

// TestDaily_VenueThresholdWinsOverThePlatformDefault: a venue configured with a
// 400 ₸ threshold is paid a 400 ₸ balance that the 10 000 ₸ platform default
// would have rolled over.
//
// MUTATION CHECK: making Tick ignore the per-venue settings (always using
// d.uc.cfg.PlatformPolicy) rolls the balance over and fails this test.
func TestDaily_VenueThresholdWinsOverThePlatformDefault(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 40_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))
	// This venue wants its money as soon as there is 400 ₸ of it.
	h.setVenuePolicy(rid, ptrInt64(40_000), nil)

	res, err := h.daily(DailyConfig{}, now).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 1 || res.RolledOver != 0 {
		t.Fatalf("the venue's own 400 ₸ threshold must win over the platform's 10 000 ₸: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 || list[0].GrossAmountMinor != 40_000 {
		t.Fatalf("expected one 400 ₸ payout, got %+v", list)
	}
	if list[0].ForcedByAge {
		t.Fatal("a payout that MET the venue's threshold must not be reported as forced by age")
	}
	if h.settings.batchCalls != 1 {
		t.Fatalf("the pass must resolve every venue's policy in ONE batch read, got %d calls",
			h.settings.batchCalls)
	}
}

// TestDaily_VenueThresholdCanAlsoBeHigherThanThePlatformDefault is the other
// direction: an override is a real override, not just a way to go lower.
func TestDaily_VenueThresholdCanAlsoBeHigherThanThePlatformDefault(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	// 20 000 ₸ — above the 10 000 ₸ platform default, below this venue's own
	// 50 000 ₸ threshold, so it must roll over.
	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))
	h.setVenuePolicy(rid, ptrInt64(5_000_000), nil)

	res, err := h.daily(DailyConfig{}, now).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("a venue threshold ABOVE the platform default must also apply: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}
}

// TestDaily_NilVenueOverrideFallsBackToThePlatformDefault: a settings ROW that
// exists but leaves min_payout_minor NULL must not be read as "threshold 0".
// The fallback is per FIELD — this venue overrides only its hold window.
//
// MUTATION CHECK: collapsing PayoutSettings.MinPayoutMinor from a pointer to a
// plain int64 (nil becoming 0) makes this test generate a payout and fail.
func TestDaily_NilVenueOverrideFallsBackToThePlatformDefault(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 40_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))
	// Hold window overridden, threshold deliberately left to the platform.
	h.setVenuePolicy(rid, nil, ptrInt(30))

	res, err := h.daily(DailyConfig{}, now).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("a NULL threshold must fall back to the platform's 10 000 ₸: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}
}

// TestDaily_VenueWithNoSettingsRowUsesThePlatformPolicy — the common case, and
// the one that must not cost anything: no row at all means the platform policy,
// not an error and not a zero policy.
func TestDaily_VenueWithNoSettingsRowUsesThePlatformPolicy(t *testing.T) {
	h := newHarnessWithConfig(Config{
		PlatformPolicy: domain.PayoutPolicy{MinPayoutMinor: 100_000, MaxHoldDays: 7},
	})
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	// 1 500 ₸ against a configured 1 000 ₸ platform threshold: paid, and no
	// settings row exists for this venue.
	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 150_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))
	res, err := h.daily(DailyConfig{}, now).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 1 {
		t.Fatalf("expected the configured platform threshold to apply, generated=%d", res.Generated)
	}
	if _, exists := h.settings.m[rid]; exists {
		t.Fatal("the test venue must have NO settings row — otherwise it is not testing the fallback")
	}
}

// TestDaily_UnreadablePolicyAbortsThePassInsteadOfPayingOnDefaults: if the
// settings read fails, every venue's override is invisible. Paying anyway would
// silently apply a policy nobody chose, so the pass must abort and retry.
func TestDaily_UnreadablePolicyAbortsThePassInsteadOfPayingOnDefaults(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))
	h.settings.err = errors.New("settings table unreachable")

	res, err := h.daily(DailyConfig{}, now).Tick(context.Background())
	if err == nil {
		t.Fatal("an unreadable payout policy must fail the pass, not fall through to defaults")
	}
	if res.Generated != 0 {
		t.Fatalf("no payout may be generated under an unknown policy, got %d", res.Generated)
	}
	if list, _ := h.payouts.List(context.Background(), rid, 10); len(list) != 0 {
		t.Fatalf("expected no payouts, got %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// 2. Forced by age
// ---------------------------------------------------------------------------

// TestDaily_ForcedByAgePaysOnceAndCannotDoublePay is the heart of max_hold_days.
//
// A venue takes 400 ₸ on 24.07 and nothing afterwards: it will NEVER reach the
// 10 000 ₸ threshold, so without the hold window its money would sit with us
// forever. With a 7-day window the pass that settles 31.07 pays it out — once —
// and marks the payout as forced by age.
//
// The second half is the double-pay guard: a ledger entry dated INSIDE the
// forced period that arrives afterwards must not produce a second payout for
// that same venue-day. That is uq_payouts_venue_period (migration 0052), which
// the forced path goes through unchanged.
//
// MUTATION CHECK (age rule): deleting the heldDays branch in runVenue makes the
// 01.08 pass roll over instead of paying, and the test fails with 0 payouts.
// MUTATION CHECK (double-pay guard): removing the period mirror from
// fakePayouts.Create makes the late entry produce a second payout and the test
// fails with 2.
func TestDaily_ForcedByAgePaysOnceAndCannotDoublePay(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	booked := time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 40_000, booked))
	h.setVenuePolicy(rid, nil, ptrInt(7))

	// 31.07 09:00 settles 30.07 — the money is 6 whole days old, still inside
	// the window, so it rolls over one more time.
	res, err := h.daily(DailyConfig{}, time.Date(2026, 7, 31, 9, 0, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("day-6 tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("money held 6 of 7 days must still roll over: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}

	// 01.08 09:00 settles 31.07 — 7 whole days. The window is up.
	res, err = h.daily(DailyConfig{}, time.Date(2026, 8, 1, 9, 0, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("day-7 tick: %v", err)
	}
	if res.Generated != 1 || res.Forced != 1 {
		t.Fatalf("money held the full window must be paid out: generated=%d forced=%d rolled=%d",
			res.Generated, res.Forced, res.RolledOver)
	}

	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 {
		t.Fatalf("expected exactly one forced payout, got %d", len(list))
	}
	forced := list[0]
	if !forced.ForcedByAge {
		t.Fatal("a payout produced below the threshold by the hold window must be marked ForcedByAge — " +
			"the statement has to explain why a small payout paid a 300 ₸ fee")
	}
	if forced.GrossAmountMinor != 40_000 {
		t.Fatalf("expected the whole held balance (40 000), got %d", forced.GrossAmountMinor)
	}
	// The fee is paid, deliberately: that is the trade the setting makes.
	if forced.FeeMinor != 30_000 {
		t.Fatalf("a forced payout still pays the acquirer's floor, expected 30 000, got %d", forced.FeeMinor)
	}
	if forced.FeeBearer != domain.PayoutFeeBearerPlatform {
		t.Fatalf("the payout fee is the platform's cost, got bearer %q", forced.FeeBearer)
	}
	if forced.AmountMinor != 40_000 {
		t.Fatalf("with the platform bearing the fee the venue must receive the full 40 000, got %d",
			forced.AmountMinor)
	}
	if forced.PeriodDate == nil || forced.PeriodDate.Format(time.DateOnly) != "2026-07-31" {
		t.Fatalf("a forced payout must still carry the venue-local period it settled, got %v", forced.PeriodDate)
	}

	// A capture dated inside 31.07 lands late, after that day was forced out.
	late := owedAt(h, 10_000, time.Date(2026, 7, 31, 22, 0, 0, 0, almaty))
	bal := h.owed.byRestaurant[rid][0]
	bal.Entries = append(bal.Entries, late)
	bal.AmountMinor += late.AmountSignedMinor
	h.owed.byRestaurant[rid] = []domain.OwedBalance{bal}
	h.setVenuePolicy(rid, ptrInt64(1), nil) // even a threshold of 1 tiyn must not re-open the day

	res, err = h.daily(DailyConfig{}, time.Date(2026, 8, 1, 9, 30, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("late-entry tick: %v", err)
	}
	if res.Generated != 0 {
		t.Fatalf("the venue-day was already paid; a late entry must not create a second payout, generated=%d",
			res.Generated)
	}
	if list, _ := h.payouts.List(context.Background(), rid, 10); len(list) != 1 {
		t.Fatalf("the venue was paid twice for 2026-07-31: %d payouts", len(list))
	}
	if h.items.isClaimed(late.LedgerEntryID) {
		t.Fatal("the refused payout must leave the late entry unclaimed so tomorrow's pass can pay it")
	}
}

// TestDaily_MaxHoldDaysZeroNeverForces: 0 is an explicit, distinct policy —
// "roll over indefinitely" — and must not be confused with "unset".
func TestDaily_MaxHoldDaysZeroNeverForces(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 40_000, time.Date(2026, 1, 1, 18, 0, 0, 0, almaty)))
	h.setVenuePolicy(rid, nil, ptrInt(0))

	// Half a year later the money is still below the threshold.
	res, err := h.daily(DailyConfig{}, time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("max_hold_days=0 must never force a payout: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}
}

// TestDaily_PlatformMaxHoldDaysAppliesWithoutAnOverride: the env default (7
// days) is a real, enforced rule, not just documentation for the override.
func TestDaily_PlatformMaxHoldDaysAppliesWithoutAnOverride(t *testing.T) {
	h := newHarnessWithConfig(Config{
		PlatformPolicy: domain.PayoutPolicy{MinPayoutMinor: 1_000_000, MaxHoldDays: 7},
	})
	almaty := mustLoad(t, tzAlmaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 40_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty)))

	res, err := h.daily(DailyConfig{}, time.Date(2026, 8, 1, 9, 0, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Forced != 1 {
		t.Fatalf("the platform hold window must apply to a venue with no override, forced=%d", res.Forced)
	}
	if list, _ := h.payouts.List(context.Background(), rid, 10); len(list) != 1 || !list[0].ForcedByAge {
		t.Fatalf("expected one age-forced payout, got %+v", list)
	}
}

// TestDaily_UnknownMoneyAgeNeverForcesAPayout: an owed balance whose entries
// carry no timestamp must roll over, not be force-paid. An unknown age has to
// fail towards keeping the money unclaimed (recoverable) rather than towards
// spending a 300 ₸ fee on a guess (not recoverable).
func TestDaily_UnknownMoneyAgeNeverForcesAPayout(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)

	rid := uuid.New()
	if err := h.dest.Upsert(context.Background(), &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
	}); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	// An entry with a ZERO CreatedAt — the age is unknown.
	entry := domain.OwedEntry{LedgerEntryID: uuid.New(), AmountSignedMinor: 40_000, Currency: domain.CurrencyKZT}
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: 40_000,
		Entries: []domain.OwedEntry{entry},
	}}
	h.owed.ids = append(h.owed.ids, rid)
	h.venues.tz[rid] = tzAlmaty

	res, err := h.daily(DailyConfig{}, time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("an unknown money age must roll over, not force: generated=%d rolled=%d",
			res.Generated, res.RolledOver)
	}
}

// TestHeldDays_CountsCalendarDaysAcrossDST: the hold window is counted in
// venue-local CALENDAR days, so a 23- or 25-hour day cannot shift the answer.
func TestHeldDays_CountsCalendarDaysAcrossDST(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	// 2026-03-29 is Berlin's spring-forward day (a 23-hour local day).
	period := lastCompletedLocalDay(time.Date(2026, 4, 1, 10, 0, 0, 0, berlin), berlin) // settles 31.03
	oldest := time.Date(2026, 3, 28, 23, 30, 0, 0, berlin)
	if got := heldDays(oldest, berlin, period); got != 3 {
		t.Fatalf("28.03 -> period 31.03 is 3 calendar days even across the DST shift, got %d", got)
	}
	// Money dated inside the settled day itself is 0 days old.
	if got := heldDays(time.Date(2026, 3, 31, 12, 0, 0, 0, berlin), berlin, period); got != 0 {
		t.Fatalf("money booked in the settled day is 0 days held, got %d", got)
	}
	// Money dated after the period (clock skew / backdated import) is not old.
	if got := heldDays(time.Date(2026, 4, 5, 12, 0, 0, 0, berlin), berlin, period); got != 0 {
		t.Fatalf("money dated after the settled period must not read as old, got %d", got)
	}
}
