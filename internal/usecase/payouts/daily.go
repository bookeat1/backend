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
//
// Safe to run twice, safe to restart: every pass recomputes from the ledger and
// every write is guarded by a DB constraint.
type DailyRunner struct {
	uc     *UseCase
	owed   domain.OwedReader
	venues domain.PayoutVenueReader
	dest   domain.PayoutDestinationRepository
	cfg    DailyConfig
	log    *slog.Logger
	now    func() time.Time
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
	// MinPayoutMinor is the amount below which a venue's money rolls into the
	// next day instead of being paid. See defaultMinPayoutMinor for the
	// reasoning behind the default.
	MinPayoutMinor int64
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
	// defaultMinPayoutMinor — 10 000 ₸ (1 000 000 tiyn).
	//
	// The 300 ₸ floor makes the fee a percentage that falls as the payout
	// grows: 300 ₸ on 10 000 ₸ is 3.0%, on 5 000 ₸ it is 6%, on 1 000 ₸ it is
	// 30%. 3% is the worst case the owner already accepted when choosing daily
	// batching over per-booking payouts, so 10 000 ₸ is the threshold that
	// holds the cost at that accepted ceiling without holding a venue's money
	// longer than necessary.
	//
	// The alternative worth naming: at 15 790 ₸ the 1.9% rate finally exceeds
	// the floor, so a threshold there would mean NEVER paying the floor premium
	// at all — cheapest for the platform, but it delays small venues' money
	// noticeably. That is an owner decision, listed in the PR notes.
	defaultMinPayoutMinor int64 = 1_000_000
	// defaultTimezoneFallback matches bookings' own fallback (spec: the
	// platform operates in Kazakhstan).
	defaultTimezoneFallback = "Asia/Almaty"
)

func (c DailyConfig) withDefaults() DailyConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultDailyTickInterval
	}
	if c.MinPayoutMinor <= 0 {
		c.MinPayoutMinor = defaultMinPayoutMinor
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
		dest:   uc.destinations,
		cfg:    cfg.withDefaults(),
		log:    log,
		now:    time.Now,
	}
}

// DailyResult reports what one pass did. Every venue lands in exactly one
// bucket per currency, so the numbers add up in a log line.
type DailyResult struct {
	Venues     int // venues with an unpaid balance this pass looked at
	Generated  int // payouts created
	Sent       int // payouts dispatched (SendEnabled only)
	RolledOver int // balances left unclaimed because they were below the minimum
	Skipped    int // already paid for the period, or no destination configured
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
			if res.Generated > 0 || res.RolledOver > 0 {
				d.log.Info("daily payout pass",
					"venues", res.Venues, "generated", res.Generated, "sent", res.Sent,
					"rolled_over", res.RolledOver, "skipped", res.Skipped)
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

	for _, id := range ids {
		res.Venues++
		if err := d.runVenue(ctx, id, zones[id], &res); err != nil {
			d.log.Error("daily payout for venue failed, other venues continue",
				"restaurant_id", id, "err", err.Error())
		}
	}
	return res, nil
}

// runVenue settles one venue's last completed local day.
func (d *DailyRunner) runVenue(ctx context.Context, restaurantID uuid.UUID, tz string, res *DailyResult) error {
	loc := d.location(tz, restaurantID)
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
		if bal.AmountMinor < d.cfg.MinPayoutMinor {
			// ROLL OVER, by doing nothing: no payout row, no claim, so these
			// exact ledger entries are still unclaimed tomorrow and the next
			// pass sees them again together with the new day's money. Nothing
			// to lose, nothing to double-count.
			res.RolledOver++
			d.log.Info("venue balance below the payout minimum, rolling into the next day",
				"restaurant_id", restaurantID, "amount_minor", bal.AmountMinor,
				"minimum_minor", d.cfg.MinPayoutMinor, "currency", string(bal.Currency),
				"period", period.Date.Format(time.DateOnly))
			continue
		}

		p, err := d.uc.createOnePayout(ctx, restaurantID, dest, bal, &period)
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

// location resolves a venue's IANA zone, falling back to the configured
// platform default. A zone the runtime cannot load is a data problem, not a
// reason to pay the venue on the wrong day — it is logged and the fallback is
// used, and if even the fallback fails we use UTC rather than time.Local (the
// server's zone must never silently decide a venue's day).
func (d *DailyRunner) location(tz string, restaurantID uuid.UUID) *time.Location {
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
		d.log.Warn("venue has an unloadable timezone, using the platform fallback",
			"restaurant_id", restaurantID, "timezone", tz, "fallback", d.cfg.TimezoneFallback)
	}
	if loc, err := time.LoadLocation(d.cfg.TimezoneFallback); err == nil {
		return loc
	}
	d.log.Error("payout timezone fallback is unloadable, settling on UTC days",
		"fallback", d.cfg.TimezoneFallback)
	return time.UTC
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
