package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//     repository's ClaimUnpublished), so the claim itself is a short DB-only
//     transaction — no external push send is ever made while a row lock or the
//     claim transaction is held open.
//   - PROCESS outside that transaction: each notifier makes its own network
//     calls with no database lock held.
//   - MARK processed in a second short transaction, only for events every
//     interested notifier durably handled. An event whose send failed is left
//     unpublished and retried on the next tick.
//
// At-least-once with dedupe: a failed send on one subscription leaves the whole
// event unpublished, so the next tick re-runs it — but the notifier's own
// per-subscription delivery ledger (domain.NotificationDeliveryRepository)
// skips the subscriptions that already got it, so a redelivery never
// double-notifies the same subscription for the same booking.
//
// Draining discipline: the outbox holds every booking event type (confirmed,
// cancelled, ...), not only booking.created. The dispatcher marks an event with
// NO interested notifier as processed too — otherwise ClaimUnpublished, which
// orders by created_at, would keep re-claiming the same oldest un-notified
// events every tick and never reach the ones a channel does care about. This
// makes the dispatcher the outbox's sole drainer: a channel added later reacts
// to FUTURE events, not ones already drained (documented in the PR).
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
}

const (
	defaultDispatchTickInterval = 15 * time.Second
	defaultDispatchBatchSize    = 100
)

func (c DispatcherConfig) withDefaults() DispatcherConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultDispatchTickInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultDispatchBatchSize
	}
	return c
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
	Retry      int // events a send failed on → left unpublished for the next tick
	Poison     int // events with an undecodable payload → marked published to avoid a loop
}

func (r TickResult) attrs() []any {
	return []any{
		slog.Int("dispatched", r.Dispatched), slog.Int("drained", r.Drained),
		slog.Int("retry", r.Retry), slog.Int("poison", r.Poison),
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
	//    LOCKED lives in ClaimUnpublished). No send happens while it is open.
	var events []domain.BookingOutboxEvent
	if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
		var e error
		events, e = d.outbox.ClaimUnpublished(ctx, d.cfg.BatchSize)
		return e
	}); err != nil {
		return res, fmt.Errorf("claim outbox events: %w", err)
	}
	if len(events) == 0 {
		return res, nil
	}

	// 2. Process outside any transaction. Collect the ids to mark processed.
	var publishedIDs []uuid.UUID
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
		allOK := true
		for _, n := range interested {
			if err := n.Notify(ctx, nev); err != nil {
				allOK = false
				d.log.Error("notifier failed, event left for retry",
					slog.String("channel", string(n.Channel())),
					slog.String("outbox_event_id", ev.ID.String()),
					slog.String("booking_id", ev.BookingID.String()),
					slog.String("error", err.Error()))
			}
		}
		if allOK {
			publishedIDs = append(publishedIDs, ev.ID)
			res.Dispatched++
		} else {
			res.Retry++
		}
	}

	// 3. Mark the fully-processed events published in a second short tx.
	if len(publishedIDs) > 0 {
		if err := d.tx.WithinTx(ctx, func(ctx context.Context) error {
			return d.outbox.MarkPublished(ctx, publishedIDs, now)
		}); err != nil {
			return res, fmt.Errorf("mark outbox events published: %w", err)
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
