package eventrecurrence

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Generator materialises recurrence rules into real `events` rows for a rolling
// window ahead, and tops that window up on every tick. It is the whole reason
// the rules exist: an occurrence must be a real row, because tickets, capacity,
// the home feed and the moderation queue all hang off an event id and cannot be
// attached to a date computed on the fly.
//
// Three properties it is built to keep, in order of how much damage their
// absence would do:
//
//  1. IT ONLY EVER INSERTS. It never updates, never deletes, and never
//     reconciles an existing occurrence against the rule's template. So an
//     occurrence the venue cancelled (status hidden), retitled, moved by an
//     hour or sold tickets for is safe from the next pass by construction —
//     there is no code path that could touch it. The one case an existing row
//     cannot protect (a HARD-DELETED occurrence, whose slot is free again) is
//     covered by the tombstones in event_recurrence_skips, written by
//     usecase/events when a generated occurrence is deleted or moved.
//
//  2. IT IS IDEMPOTENT AND CONCURRENCY-SAFE, in the database rather than in
//     this code: the insert carries ON CONFLICT (recurrence_id, starts_at) DO
//     NOTHING against a unique index. A second pass inserts nothing; two
//     workers passing at the same instant produce exactly one row per slot. No
//     leader election, no advisory lock, no "check then insert" window.
//
//  3. IT NEVER GUESSES A TIMEZONE. A rule's own zone wins; otherwise the
//     venue's; only if the venue has none does the platform fallback apply. A
//     STORED-BUT-UNUSABLE zone is an error that skips THAT rule and lets the
//     rest of the pass proceed — never a silent fallback to the platform zone,
//     which is how a whole series quietly moves by an hour (see
//     domain/venue_timezone.go and the payouts pass, which refuses the same way).
type Generator struct {
	repo domain.EventRecurrenceRepository
	cfg  GeneratorConfig
	log  *slog.Logger
	now  func() time.Time
}

// GeneratorConfig tunes the generator. Zero values fall back to defaults.
type GeneratorConfig struct {
	// TickInterval is the pause between passes. env: EVENT_RECURRENCE_TICK_INTERVAL
	TickInterval time.Duration
	// Window is how far ahead occurrences are materialised. A rolling window
	// rather than "generate everything to the until-date": an open-ended weekly
	// rule would otherwise generate to the heat death of the Афиша, and every
	// one of those rows is a real event a guest could stumble on.
	// env: EVENT_RECURRENCE_WINDOW
	Window time.Duration
	// BatchSize caps how many rules one keyset page reads.
	// env: EVENT_RECURRENCE_BATCH_SIZE
	BatchSize int
	// TimezoneFallback is the platform zone used ONLY when neither the rule nor
	// the venue names one. env: BOOKING_TIMEZONE_FALLBACK (shared with bookings
	// and payouts — a venue's day means the same thing everywhere).
	TimezoneFallback string
}

const (
	// Five minutes, not an hour: a venue that has just created a rule expects
	// the Афиша to fill up while it is still looking at the screen. A pass over
	// a handful of rules whose slots all already exist is a single INSERT ...
	// ON CONFLICT DO NOTHING per rule that writes nothing, so a short tick costs
	// almost nothing.
	defaultTickInterval = 5 * time.Minute
	// Eight weeks ahead, the owner's default: far enough that the Афиша never
	// looks empty, short enough that a rule edit reaches most of what guests
	// will actually see.
	defaultWindow           = 8 * 7 * 24 * time.Hour
	defaultBatchSize        = 100
	defaultTimezoneFallback = "UTC"
)

func (c GeneratorConfig) withDefaults() GeneratorConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.Window <= 0 {
		c.Window = defaultWindow
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.TimezoneFallback == "" {
		c.TimezoneFallback = defaultTimezoneFallback
	}
	return c
}

// NewGenerator constructs the occurrence generator.
func NewGenerator(repo domain.EventRecurrenceRepository, cfg GeneratorConfig, log *slog.Logger) *Generator {
	return &Generator{repo: repo, cfg: cfg.withDefaults(), log: log, now: time.Now}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick, never fatal — same contract as the payments reconciler and the
// ticket sweeper. A graceful shutdown returns nil so cmd/worker exits 0.
func (g *Generator) Run(ctx context.Context) error {
	t := time.NewTicker(g.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := g.Generate(ctx); err != nil {
				g.log.Error("event_recurrence.generate_failed", slog.String("error", err.Error()))
			} else if n > 0 {
				g.log.Info("event_recurrence.generated", slog.Int("occurrences", n))
			}
		}
	}
}

// Generate runs one pass over every active rule and returns how many
// occurrences it actually created.
//
// A rule whose zone cannot be resolved is SKIPPED with an error log, and the
// pass continues: one venue with a broken timezone row must not stop every
// other venue's Афиша from filling, exactly as one unpayable venue does not
// stop the daily payout pass.
func (g *Generator) Generate(ctx context.Context) (int, error) {
	from := g.now()
	to := from.Add(g.cfg.Window)

	created := 0
	after := uuid.Nil
	for {
		rules, err := g.repo.ListActive(ctx, after, g.cfg.BatchSize)
		if err != nil {
			return created, err
		}
		if len(rules) == 0 {
			return created, nil
		}
		for i := range rules {
			rule := rules[i]
			after = rule.ID
			n, err := g.generateOne(ctx, rule, from, to)
			if err != nil {
				return created, err
			}
			created += n
		}
		if len(rules) < g.cfg.BatchSize {
			return created, nil
		}
	}
}

func (g *Generator) generateOne(ctx context.Context, rule domain.ActiveEventRecurrence, from, to time.Time) (int, error) {
	loc, err := g.location(rule)
	if err != nil {
		g.log.Error("event_recurrence.timezone_unusable",
			slog.String("recurrence_id", rule.ID.String()),
			slog.String("restaurant_id", rule.RestaurantID.String()),
			slog.String("error", err.Error()))
		return 0, nil
	}
	slots := rule.Occurrences(loc, from, to)
	if len(slots) == 0 {
		return 0, nil
	}
	n, err := g.repo.InsertOccurrences(ctx, &rule.EventRecurrence, slots)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		g.log.Info("event_recurrence.occurrences_created",
			slog.String("recurrence_id", rule.ID.String()),
			slog.Int("created", n))
	}
	return n, nil
}

// location resolves the zone this rule's wall-clock time is measured in.
//
// The precedence — rule override, then venue, then platform fallback — is the
// only place Asia/Almaty could have been hardcoded, and deliberately is not:
// the fallback is an env var (BOOKING_TIMEZONE_FALLBACK) whose default happens
// to be Asia/Almaty today, and a venue that names its own zone always wins over
// it. A zone that IS stored but cannot be understood returns an error instead
// of falling back, because "this venue has no zone" and "this venue's zone is
// broken data" are different facts and must not produce the same answer.
func (g *Generator) location(rule domain.ActiveEventRecurrence) (*time.Location, error) {
	if rule.Timezone != "" {
		return domain.LoadVenueLocation(rule.Timezone)
	}
	if rule.VenueTimezone != "" {
		return domain.LoadVenueLocation(rule.VenueTimezone)
	}
	return domain.LoadVenueLocation(g.cfg.TimezoneFallback)
}
