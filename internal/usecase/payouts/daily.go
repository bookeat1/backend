package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// DailyRunner is the scheduled end-of-day payout pass: ONE payout per venue per
// day, covering that venue's settled-and-unclaimed money up to the end of the
// venue's own local day (owner decision, 25.07.2026 — payouts used to be an
// on-demand superadmin action with nothing enforcing a cadence).
//
// Three properties it is built for, in order of how much money they protect:
//
//  1. A venue is NEVER paid twice for the same period. That is a partial UNIQUE
//     index on (restaurant_id, currency, period_date) over the non-failed rows
//     (migration 0052), not a check in this file: two worker instances ticking
//     at the same second, a worker restarted mid-pass, or an operator running
//     the pass by hand all collide in the database and the loser skips. A
//     FAILED payout drops out of that index, so a declined send releases both
//     its claimed ledger entries and its day — a failure must not silently
//     postpone the venue's money.
//  2. The day is the VENUE's local day. A venue in Almaty and a venue in
//     Istanbul close their books at different instants; settling both on the
//     server's midnight would put an evening's takings in the wrong period for
//     one of them.
//  3. Money below the payout minimum ROLLS OVER instead of being paid. With
//     FreedomPay's 300 ₸ floor a 500 ₸ payout costs 60% of itself. Rolling over
//     is implemented by doing NOTHING — no payout row, no claim — so the same
//     ledger entries are simply still unclaimed tomorrow and are picked up by
//     the next pass. There is no carry-forward record that could be lost,
//     double-counted, or drift out of sync with the ledger.
//  4. That roll-over is BOUNDED. Once a venue's oldest unpaid money has been
//     held for its full max-hold window, the pass pays out anyway — below the
//     threshold, fee and all. A venue whose turnover never reaches the minimum
//     must not have its money held indefinitely; the fee is the accepted price
//     of that, and the payout is marked ForcedByAge so the statement says why.
//
// The threshold and the hold window are PER VENUE (restaurant_payout_settings,
// migration 0053) with the platform env values as the fallback — resolved once
// per pass through UseCase.effectivePolicyFor, the same function the venue-
// facing read endpoint uses.
//
// Safe to run twice, safe to restart: every pass recomputes from the ledger and
// every write is guarded by a DB constraint.
type DailyRunner struct {
	uc       *UseCase
	owed     domain.OwedReader
	venues   domain.PayoutVenueReader
	settings domain.PayoutSettingsRepository
	dest     domain.PayoutDestinationRepository
	cfg      DailyConfig
	log      *slog.Logger
	now      func() time.Time
}

// DailyConfig is the schedule and the money thresholds of the daily pass.
type DailyConfig struct {
	// TickInterval is how often the pass looks for venues whose local day has
	// ended. It is NOT the payout cadence: the cadence is one per venue per
	// local day, enforced in the database. Ticking more often than daily is
	// what lets venues in different timezones each be settled shortly after
	// THEIR midnight, and what lets a worker that was down at midnight catch up
	// on its next tick instead of skipping a day.
	TickInterval time.Duration
	// SendEnabled controls whether a generated payout is also dispatched.
	// Default false: generating is safe (no money moves, the venue's statement
	// is correct), sending is not, and it also needs the FreedomPay payout
	// product to be live. With it off the pass still produces pending payouts,
	// which an operator can inspect and release.
	SendEnabled bool
	// The payout threshold and the max-hold window are NOT here: they moved to
	// Config.PlatformPolicy, because they are now per-venue overridable and the
	// runner must resolve exactly the same effective policy the API reports.
	//
	// TimezoneFallback is the IANA zone used for a venue with no timezone of
	// its own. Explicit rather than time.Local: the server's zone is an
	// accident of deployment and must never decide when a venue gets paid.
	TimezoneFallback string
}

const (
	// defaultDailyTickInterval — 15 minutes. Fine-grained enough that a venue is
	// settled within a quarter of an hour of its local midnight, cheap enough
	// that a tick with nothing to do is two small queries.
	defaultDailyTickInterval = 15 * time.Minute
	// The payout threshold's default and its reasoning live next to the rest of
	// the money policy, in ports.go (defaultMinPayoutMinor / defaultMaxHoldDays).
	//
	// The threshold alternative worth naming: at 15 790 ₸ the 1.9% rate finally
	// exceeds the floor, so a threshold there would mean NEVER paying the floor
	// premium at all — cheapest for the platform, but it delays small venues'
	// money noticeably. That is an owner decision, listed in the PR notes.
	//
	// defaultTimezoneFallback matches bookings' own fallback (spec: the
	// platform operates in Kazakhstan).
	defaultTimezoneFallback = "Asia/Almaty"
)

func (c DailyConfig) withDefaults() DailyConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultDailyTickInterval
	}
	if c.TimezoneFallback == "" {
		c.TimezoneFallback = defaultTimezoneFallback
	}
	return c
}

// NewDailyRunner builds the scheduled payout pass over an already-built
// UseCase, reusing its repositories, its fee policy and its send path so a
// scheduled payout and a manual one are the same object with the same
// guarantees.
func NewDailyRunner(uc *UseCase, venues domain.PayoutVenueReader, cfg DailyConfig, log *slog.Logger) *DailyRunner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &DailyRunner{
		uc:     uc,
		owed:   uc.owed,
		venues: venues,
		// Taken from the usecase, not from a parameter: the manual path and the
		// scheduled path must read the same per-venue policy.
		settings: uc.settings,
		dest:     uc.destinations,
		cfg:      cfg.withDefaults(),
		log:      log,
		now:      time.Now,
	}
}

// DailyResult reports what one pass did. Every venue lands in exactly one
// bucket per currency, so the numbers add up in a log line.
type DailyResult struct {
	Venues     int // venues with an unpaid balance this pass looked at
	Generated  int // payouts created (threshold-driven AND age-forced)
	Forced     int // of those, created by the max-hold rule while below threshold
	Sent       int // payouts dispatched (SendEnabled only)
	RolledOver int // balances left unclaimed because they were below the minimum
	Skipped    int // already paid for the period, or no destination configured
	// Stuck counts balances that are PAST their hold limit and still cannot be
	// paid, because the acquirer's fee would be at least the balance itself.
	// They keep waiting. This number is the one a human has to look at: it is
	// money the platform is holding and cannot economically send.
	Stuck int
}

// Run loops until ctx is cancelled, exactly like the other workers.
func (d *DailyRunner) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			res, err := d.Tick(ctx)
			if err != nil {
				d.log.Error("daily payout tick failed", "err", err.Error())
				continue
			}
			if res.Generated > 0 || res.RolledOver > 0 || res.Stuck > 0 {
				d.log.Info("daily payout pass",
					"venues", res.Venues, "generated", res.Generated, "forced_by_age", res.Forced,
					"sent", res.Sent, "rolled_over", res.RolledOver, "skipped", res.Skipped,
					"stuck", res.Stuck)
			}
		}
	}
}

// Tick runs one pass over every venue that currently has unpaid money.
//
// A single venue's failure (unreadable destination, an acquirer that timed out,
// a broken timezone string) is logged and does NOT abort the pass: one venue
// must not be able to stop every other venue from being paid. The pass is
// re-entrant, so whatever failed is retried on the next tick.
func (d *DailyRunner) Tick(ctx context.Context) (DailyResult, error) {
	var res DailyResult

	ids, err := d.owed.OwedRestaurantIDs(ctx)
	if err != nil {
		return res, fmt.Errorf("list venues with unpaid balance: %w", err)
	}
	if len(ids) == 0 {
		return res, nil
	}
	zones, err := d.venues.TimezonesFor(ctx, ids)
	if err != nil {
		return res, fmt.Errorf("resolve venue timezones: %w", err)
	}
	// One query for every owed venue's policy, same shape as the timezone read:
	// a pass over N venues must not do N settings lookups. A venue with no
	// overrides is simply absent from the map.
	settings, err := d.settingsFor(ctx, ids)
	if err != nil {
		// The policy decides WHEN money leaves. Unlike a single venue's failure
		// this is not isolatable — running the whole pass on platform defaults
		// would ignore every venue's override — so the pass aborts and retries
		// on the next tick rather than paying under a policy nobody chose.
		return res, fmt.Errorf("resolve venue payout settings: %w", err)
	}

	for _, id := range ids {
		res.Venues++
		policy := d.uc.cfg.PlatformPolicy
		if s, ok := settings[id]; ok {
			policy = d.uc.effectivePolicyFor(&s)
		}
		if err := d.runVenue(ctx, id, zones[id], policy, &res); err != nil {
			d.log.Error("daily payout for venue failed, other venues continue",
				"restaurant_id", id, "err", err.Error())
		}
	}
	return res, nil
}

// settingsFor batch-reads the per-venue payout overrides. With no repository
// wired every venue follows the platform policy — the exact behaviour before
// per-venue settings existed, expressed as an empty map rather than a branch in
// the loop below.
func (d *DailyRunner) settingsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.PayoutSettings, error) {
	if d.settings == nil {
		return map[uuid.UUID]domain.PayoutSettings{}, nil
	}
	return d.settings.ForRestaurants(ctx, ids)
}

// runVenue settles one venue's last completed local day under the EFFECTIVE
// policy for that venue (its own overrides on top of the platform default).
func (d *DailyRunner) runVenue(ctx context.Context, restaurantID uuid.UUID, tz string, policy domain.PayoutPolicy, res *DailyResult) error {
	loc, err := d.location(tz)
	if err != nil {
		// No payout, no claim, no period consumed: the venue's money simply
		// stays owed and is paid in full once the zone is corrected. Refusing is
		// the cheap failure here — settling on a guessed zone would write a
		// period_date that the once-per-venue-day unique index then makes
		// permanent, so the wrong day could not be re-settled afterwards.
		return fmt.Errorf("resolve venue timezone: %w", err)
	}
	period := lastCompletedLocalDay(d.now(), loc)

	dest, err := d.dest.Get(ctx, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The venue is owed money but has told us nowhere to send it. Not
			// an error of ours: the money stays unclaimed and is paid in full
			// the first day after a destination is configured.
			res.Skipped++
			d.log.Warn("venue has an unpaid balance but no payout destination, money stays owed",
				"restaurant_id", restaurantID)
			return nil
		}
		return fmt.Errorf("read payout destination: %w", err)
	}

	balances, err := d.owed.OwedForRestaurantUpTo(ctx, restaurantID, period.EndAt)
	if err != nil {
		return fmt.Errorf("read owed up to %s: %w", period.EndAt, err)
	}

	for _, bal := range balances {
		forced := false
		if bal.AmountMinor < policy.MinPayoutMinor {
			held := heldDays(bal.OldestEntryAt(), loc, period)
			if policy.MaxHoldDays <= 0 || held < policy.MaxHoldDays {
				// ROLL OVER, by doing nothing: no payout row, no claim, so
				// these exact ledger entries are still unclaimed tomorrow and
				// the next pass sees them again together with the new day's
				// money. Nothing to lose, nothing to double-count.
				res.RolledOver++
				d.log.Info("venue balance below the payout minimum, rolling into the next day",
					"restaurant_id", restaurantID, "amount_minor", bal.AmountMinor,
					"minimum_minor", policy.MinPayoutMinor, "held_days", held,
					"max_hold_days", policy.MaxHoldDays, "currency", string(bal.Currency),
					"period", period.Date.Format(time.DateOnly))
				continue
			}
			// The hold window is up — but paying is not automatically the right
			// answer. Owner decision (2026-07-25): never send a payout the fee
			// would eat. Below the fee itself the transfer destroys more of the
			// venue's money than holding it does (300 ₸ taken out of a 400 ₸
			// balance), so such a balance keeps waiting and is reported as
			// STUCK — a venue nobody can pay economically is an operator
			// problem, not something to burn quietly.
			fee, feeErr := domain.PayoutFee(
				domain.Money{AmountMinor: bal.AmountMinor, Currency: bal.Currency},
				d.uc.cfg.FeeBps, d.uc.cfg.FeeMinimumMinor)
			if feeErr != nil {
				return fmt.Errorf("compute payout fee for %s: %w", restaurantID, feeErr)
			}
			if bal.AmountMinor <= fee.AmountMinor {
				res.Stuck++
				d.log.Warn("venue balance held past the limit but too small to pay: the fee would exceed it",
					"restaurant_id", restaurantID, "amount_minor", bal.AmountMinor,
					"fee_minor", fee.AmountMinor, "held_days", held, "max_hold_days", policy.MaxHoldDays,
					"currency", string(bal.Currency), "period", period.Date.Format(time.DateOnly))
				continue
			}
			// The hold window is up and the payout is worth making. It is still
			// small, so the acquirer's floor is a large share of it — that is
			// the deliberate cost of the setting, and it is why the row is
			// marked, so the venue's statement can say the payout happened
			// because the money got old, not because a threshold was met.
			forced = true
			d.log.Info("venue balance below the payout minimum but held too long, paying anyway",
				"restaurant_id", restaurantID, "amount_minor", bal.AmountMinor,
				"minimum_minor", policy.MinPayoutMinor, "held_days", held,
				"max_hold_days", policy.MaxHoldDays, "currency", string(bal.Currency),
				"period", period.Date.Format(time.DateOnly))
		}

		// Whether it was the threshold or the age that triggered it, the payout
		// takes the SAME path: same period, same claim, same once-per-venue-day
		// unique index. A forced payout therefore cannot double-pay any more
		// than a normal one can — there is no second code path to get wrong.
		venuePeriod := period
		venuePeriod.ForcedByAge = forced

		p, err := d.uc.createOnePayout(ctx, restaurantID, dest, bal, &venuePeriod)
		if err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				// Either this venue-day already has a live payout, or a
				// concurrent tick claimed these ledger entries first. Both are
				// the guarantee working, not a failure.
				res.Skipped++
				d.log.Info("venue already has a payout for this period, skipping",
					"restaurant_id", restaurantID, "currency", string(bal.Currency),
					"period", period.Date.Format(time.DateOnly))
				continue
			}
			return fmt.Errorf("generate payout: %w", err)
		}
		res.Generated++
		if forced {
			res.Forced++
		}

		if !d.cfg.SendEnabled {
			continue
		}
		if _, err := d.uc.sendPayout(ctx, p.ID); err != nil {
			// An unknown outcome leaves the payout `sent` for the payout
			// reconciler — never retried blindly here, that is how money moves
			// twice. A definite decline has already released the claim and the
			// period inside sendPayout.
			d.log.Warn("scheduled payout did not complete synchronously, left for the reconciler",
				"payout_id", p.ID, "restaurant_id", restaurantID, "err", err.Error())
			continue
		}
		res.Sent++
	}
	return nil
}

// location resolves the zone this venue's payout day is measured in.
//
// Two cases, and they are NOT the same:
//
//   - the venue has no zone of its own (tz empty — PayoutVenueReader leaves such
//     a venue out of the map). The platform fallback applies; that is the
//     documented meaning of BOOKING_TIMEZONE_FALLBACK, not a guess.
//   - the venue HAS a stored zone and it is not usable ("KZT", "+06", "Local",
//     a name this host's tzdata does not know). That is a data fault, and the
//     honest answer is an error. It used to fall back to the platform zone with
//     a warning, which is the same shape of silent substitution that let the DST
//     bug hide for weeks: an Istanbul venue would have been settled on Almaty
//     days — three hours of one day's takings landing in the neighbouring
//     period, every day, with the venue's statement looking perfectly plausible.
//
// A failing fallback is also an error rather than UTC: the server's own
// preferences must never decide when a venue gets paid, and UTC is nobody's
// business day.
func (d *DailyRunner) location(tz string) (*time.Location, error) {
	if tz != "" {
		return domain.LoadVenueLocation(tz)
	}
	loc, err := time.LoadLocation(d.cfg.TimezoneFallback)
	if err != nil {
		return nil, domain.WithCode(domain.CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: platform payout timezone fallback %q is unloadable", domain.ErrValidation, d.cfg.TimezoneFallback))
	}
	return loc, nil
}

// lastCompletedLocalDay returns the venue-local day this pass settles: the last
// calendar day that has FULLY ended in loc.
//
// At any instant, that is "yesterday" in the venue's own zone, and the period
// ends at the venue's most recent local midnight. Computed by truncating to
// today's local midnight and stepping back a day — the step back goes through
// a 12-hour offset and a second truncation so a DST transition (a 23- or
// 25-hour local day) still lands on the correct calendar date rather than on
// 23:00 of the day before.
//
// Date is normalised to UTC midnight because it is a calendar LABEL stored in a
// `date` column, not an instant; EndAt is the real instant and is what bounds
// the ledger query.
func lastCompletedLocalDay(now time.Time, loc *time.Location) payoutPeriod {
	local := now.In(loc)
	todayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	prev := todayStart.Add(-12 * time.Hour)
	return payoutPeriod{
		Date:  time.Date(prev.Year(), prev.Month(), prev.Day(), 0, 0, 0, 0, time.UTC),
		EndAt: todayStart,
	}
}

// heldDays returns how many WHOLE venue-local days the venue's oldest unpaid
// money has been held by the end of the period being settled.
//
// Counted in CALENDAR days, not in elapsed hours: both sides are reduced to a
// venue-local calendar date normalised to UTC midnight, so the subtraction is
// exact and a DST transition (a 23- or 25-hour local day) cannot shift the
// answer by one. Money booked on day D therefore reads 0 when day D is settled,
// 1 on D+1, and with max_hold_days = 7 it is forced out when the pass settles
// D+7 — i.e. after seven full days of holding.
//
// A zero `oldest` means the balance carries no usable timestamp. It returns 0,
// which reads as "brand new" and so NEVER forces a payout: an unknown age must
// fail towards rolling over, not towards spending money on a fee.
func heldDays(oldest time.Time, loc *time.Location, period payoutPeriod) int {
	if oldest.IsZero() {
		return 0
	}
	local := oldest.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	held := int(period.Date.Sub(day) / (24 * time.Hour))
	if held < 0 {
		// Money dated after the period being settled (a clock skew, a
		// backdated import). Not old, by definition.
		return 0
	}
	return held
}
