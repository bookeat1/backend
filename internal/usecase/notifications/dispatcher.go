package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Dispatcher is the notification worker. Once per tick it drains the booking
// transactional outbox and fans each event out to the registered notifiers.
//
// Discipline (mirrors the payments reconciler / booking worker):
//
//   - CLAIM inside a transaction with FOR UPDATE SKIP LOCKED (the outbox
//     repository's ClaimDue), so the claim itself is a short DB-only
//     transaction — no external push send is ever made while a row lock or the
//     claim transaction is held open.
//   - PROCESS outside that transaction: each notifier makes its own network
//     calls with no database lock held.
//   - MARK processed in a second short transaction, only for events every
//     interested notifier durably handled. An event whose send failed is
//     RESCHEDULED with backoff instead of being published.
//
// At-least-once with dedupe: a failed send on one subscription leaves the whole
// event unpublished, so a later tick re-runs it — but the notifier's own
// per-subscription delivery ledger (domain.NotificationDeliveryRepository)
// skips the subscriptions that already got it, so a redelivery never
// double-notifies the same subscription for the same booking.
//
// Fairness (migration 0083). An event is still only published when every
// interested channel returned nil, but a failing event no longer sits at the
// head of the queue: each failure bumps `attempts` and sets `next_attempt_at`
// to now + an exponential backoff, and ClaimDue both skips not-yet-due rows and
// returns never-attempted events ahead of retries. That is what keeps a
// sustained outage of ONE channel (a dead Meta token, a WhatsApp rate limit)
// from starving every other channel: before this, ~BatchSize permanently
// failing events were re-read every tick and newer events were never claimed at
// all, so a WhatsApp outage silently took Telegram, web push, guest push and
// the in-app feed down with it.
//
// Giving up is explicit, never silent. After MaxAttempts the event is ABANDONED
// (abandoned_at stamped, still unpublished) and logged at ERROR with its id and
// last error, so `WHERE abandoned_at IS NOT NULL` is an exact dead-letter list a
// human can replay — clearing abandoned_at/attempts/next_attempt_at re-queues
// it, and the delivery ledger keeps the replay from double-notifying anyone.
//
// Draining discipline: the outbox holds every booking event type (confirmed,
// cancelled, ...), not only booking.created. The dispatcher marks an event with
// NO interested notifier as processed too — otherwise ClaimDue would keep
// re-claiming the same oldest un-notified events every tick and never reach the
// ones a channel does care about. This makes the dispatcher the outbox's sole
// drainer: a channel added later reacts to FUTURE events, not ones already
// drained (documented in the PR).
//
// Single-instance assumption: like the booking worker and payments reconciler,
// exactly one dispatcher process runs. The delivery ledger gives at-least-once
// dedupe against a redelivery by that one instance; it is NOT a lock against two
// dispatcher processes racing the same event (the claim's SKIP LOCKED lock is
// released when the short claim transaction commits). Running a second instance
// would need the claim-and-lease pattern instead — out of scope for increment 1.
type Dispatcher struct {
	outbox    domain.BookingOutboxRepository
	tx        domain.TxManager
	notifiers []Notifier
	cfg       DispatcherConfig
	log       *slog.Logger
	now       func() time.Time // injectable clock for tests
}

// DispatcherConfig is the worker's scheduling configuration.
type DispatcherConfig struct {
	// TickInterval is the pause between two passes. env:
	// NOTIFY_DISPATCH_TICK_INTERVAL
	TickInterval time.Duration
	// BatchSize caps how many outbox events one pass claims. env:
	// NOTIFY_DISPATCH_BATCH_SIZE
	BatchSize int
	// RetryBaseDelay is the wait after the FIRST failed attempt; it doubles
	// with every further failure. env: NOTIFY_RETRY_BASE_DELAY
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps that doubling. env: NOTIFY_RETRY_MAX_DELAY
	RetryMaxDelay time.Duration
	// MaxAttempts is the attempt budget before an event is abandoned into the
	// dead letter. env: NOTIFY_RETRY_MAX_ATTEMPTS
	MaxAttempts int
}

const (
	defaultDispatchTickInterval = 15 * time.Second
	defaultDispatchBatchSize    = 100
	defaultRetryBaseDelay       = time.Minute
	defaultRetryMaxDelay        = time.Hour
	// defaultMaxAttempts with the delays above spans ~6 hours (1+2+4+8+16+32
	// minutes, then hourly). A booking alert that has not reached the venue in
	// six hours has no operational value left — the sitting either happened or
	// did not — so past that point the honest move is the dead letter and a
	// human, not another year of retries against a dead token.
	defaultMaxAttempts = 12
)

func (c DispatcherConfig) withDefaults() DispatcherConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultDispatchTickInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultDispatchBatchSize
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = defaultRetryBaseDelay
	}
	if c.RetryMaxDelay < c.RetryBaseDelay {
		c.RetryMaxDelay = maxDuration(defaultRetryMaxDelay, c.RetryBaseDelay)
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	return c
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// backoff returns the wait after `attempts` failed attempts (attempts >= 1):
// base, 2×base, 4×base … capped at RetryMaxDelay. No jitter: exactly one
// dispatcher process runs, so there is no thundering herd to spread out, and a
// deterministic schedule is one a test can assert.
func (c DispatcherConfig) backoff(attempts int) time.Duration {
	d := c.RetryBaseDelay
	for i := 1; i < attempts; i++ {
		if d >= c.RetryMaxDelay/2 {
			return c.RetryMaxDelay
		}
		d *= 2
	}
	return d
}

// NewDispatcher builds the notification dispatcher over its notifiers.
func NewDispatcher(
	outbox domain.BookingOutboxRepository,
	tx domain.TxManager,
	cfg DispatcherConfig,
	log *slog.Logger,
	notifiers ...Notifier,
) *Dispatcher {
	return &Dispatcher{
		outbox:    outbox,
		tx:        tx,
		notifiers: notifiers,
		cfg:       cfg.withDefaults(),
		log:       log,
		now:       time.Now,
	}
}

// TickResult counts what one pass did. Zero values are the steady state.
type TickResult struct {
	Dispatched int // events every interested notifier handled → marked published
	Drained    int // events no notifier cared about → marked published (nothing to do)
	Retry      int // events a send failed on → rescheduled with backoff
	Poison     int // events with an undecodable payload → marked published to avoid a loop
	Abandoned  int // events out of attempts → dead letter (unpublished, abandoned_at set)
}

func (r TickResult) attrs() []any {
	return []any{
		slog.Int("dispatched", r.Dispatched), slog.Int("drained", r.Drained),
		slog.Int("retry", r.Retry), slog.Int("poison", r.Poison),
		slog.Int("abandoned", r.Abandoned),
	}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick — a transient database error must not kill the process.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.TickInterval)
	defer t.Stop()
	d.log.Info("notification dispatcher started",
		slog.Duration("tick", d.cfg.TickInterval),
		slog.Int("notifiers", len(d.notifiers)))
	for {
		select {
		case <-ctx.Done():
			d.log.Info("notification dispatcher stopped")
			return nil
		case <-t.C:
			res, err := d.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				d.log.Error("notification dispatcher tick failed", slog.String("error", err.Error()))
				continue
			}
			if res != (TickResult{}) {
				d.log.Info("notification dispatcher tick", res.attrs()...)
			}
		}
	}
}

// Tick runs one pass. Exported so it can be driven directly from tests.
func (d *Dispatcher) Tick(ctx context.Context) (TickResult, error) {
	now := d.now()
	var res TickResult

	// 1. Claim a batch inside a short DB-only transaction (FOR UPDATE SKIP
	//    LOCKED lives in ClaimDue). No send happens while it is open.
	var events []domain.BookingOutboxEvent
	if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
		var e error
		events, e = d.outbox.ClaimDue(ctx, d.cfg.BatchSize, now)
		return e
	}); err != nil {
		return res, fmt.Errorf("claim outbox events: %w", err)
	}
	if len(events) == 0 {
		return res, nil
	}

	// 2. Process outside any transaction. Collect the ids to mark processed and
	//    the failures to reschedule or abandon.
	var publishedIDs []uuid.UUID
	var retries, abandoned []domain.BookingOutboxFailure
	for _, ev := range events {
		interested := d.interested(ev.EventType)
		if len(interested) == 0 {
			publishedIDs = append(publishedIDs, ev.ID)
			res.Drained++
			continue
		}
		nev, err := toEvent(ev)
		if err != nil {
			// A payload that cannot be decoded can never succeed; marking it
			// processed avoids a poison-pill that blocks the whole outbox.
			d.log.Error("notification dispatcher: undecodable outbox payload, skipping",
				slog.String("outbox_event_id", ev.ID.String()), slog.String("error", err.Error()))
			publishedIDs = append(publishedIDs, ev.ID)
			res.Poison++
			continue
		}
		var failures []string
		for _, n := range interested {
			if err := n.Notify(ctx, nev); err != nil {
				failures = append(failures, string(n.Channel())+": "+err.Error())
				d.log.Error("notifier failed, event left for retry",
					slog.String("channel", string(n.Channel())),
					slog.String("outbox_event_id", ev.ID.String()),
					slog.String("booking_id", ev.BookingID.String()),
					slog.Int("attempts", ev.Attempts+1),
					slog.String("error", err.Error()))
			}
		}
		switch {
		case len(failures) == 0:
			publishedIDs = append(publishedIDs, ev.ID)
			res.Dispatched++
		case ev.Attempts+1 >= d.cfg.MaxAttempts:
			// Out of budget. Loud, attributable, and left unpublished so the row
			// itself is the dead letter — a silent drop is not an option.
			reason := strings.Join(failures, "; ")
			d.log.Error("notification event abandoned after the attempt budget ran out",
				slog.String("outbox_event_id", ev.ID.String()),
				slog.String("booking_id", ev.BookingID.String()),
				slog.String("event_type", string(ev.EventType)),
				slog.Int("attempts", ev.Attempts+1),
				slog.String("error", reason))
			abandoned = append(abandoned, domain.BookingOutboxFailure{ID: ev.ID, LastError: reason})
			res.Abandoned++
		default:
			retries = append(retries, domain.BookingOutboxFailure{
				ID:            ev.ID,
				LastError:     strings.Join(failures, "; "),
				NextAttemptAt: now.Add(d.cfg.backoff(ev.Attempts + 1)),
			})
			res.Retry++
		}
	}

	// 3. Persist the outcomes in short transactions. Each of the three is
	//    independent: a failed reschedule must not undo a publish (the ledger
	//    already stops the redelivery from double-notifying), and an abandoned
	//    event must be recorded even if a sibling statement fails.
	if len(publishedIDs) > 0 {
		if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
			return d.outbox.MarkPublished(ctx, publishedIDs, now)
		}); err != nil {
			return res, fmt.Errorf("mark outbox events published: %w", err)
		}
	}
	if len(retries) > 0 {
		if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
			return d.outbox.Reschedule(ctx, retries)
		}); err != nil {
			return res, fmt.Errorf("reschedule failed outbox events: %w", err)
		}
	}
	if len(abandoned) > 0 {
		if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
			return d.outbox.Abandon(ctx, abandoned, now)
		}); err != nil {
			return res, fmt.Errorf("abandon exhausted outbox events: %w", err)
		}
	}
	return res, nil
}

func (d *Dispatcher) interested(t domain.BookingEventType) []Notifier {
	var out []Notifier
	for _, n := range d.notifiers {
		if n.Interested(t) {
			out = append(out, n)
		}
	}
	return out
}
