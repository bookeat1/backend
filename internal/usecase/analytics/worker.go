package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Dispatcher is the analytics worker. Once per tick it walks each source outbox
// from its cursor, projects the tracked rows to PII-free events and ships them
// to Amplitude in one batch per source, then advances the cursor.
//
// Discipline:
//   - READ a bounded batch after the stored cursor (no lock, no published_at:
//     the notification dispatcher owns that marker; see the 0046 migration).
//   - SHIP the batch. On a transient failure the cursor is NOT advanced, so the
//     exact same batch is reshipped next tick; Amplitude dedupes on
//     device_id+insert_id, so a reship never double-counts.
//   - ADVANCE the cursor to the last row of the batch only after a successful
//     ship. Untracked and poison rows inside a shipped batch are skipped from
//     the payload but still passed over by the cursor, so they never re-block.
//
// Single-instance assumption: like the other workers, exactly one analytics
// process runs. Two would each hold their own cursor and double-ship, but
// Amplitude's dedupe would still collapse the duplicates.
type Dispatcher struct {
	reader  SourceReader
	cursor  CursorStore
	sender  Sender
	cfg     Config
	log     *slog.Logger
	sources []SourceName
	now     func() time.Time
}

// Config is the analytics worker's scheduling.
type Config struct {
	TickInterval time.Duration // env: ANALYTICS_DISPATCH_TICK_INTERVAL
	BatchSize    int           // env: ANALYTICS_DISPATCH_BATCH_SIZE
}

const (
	defaultAnalyticsTickInterval = 30 * time.Second
	defaultAnalyticsBatchSize    = 100
	// maxAnalyticsBatchSize keeps a single Amplitude request well under its
	// 2000-event / 1 MB ceiling regardless of a misconfigured env value.
	maxAnalyticsBatchSize = 500
)

func (c Config) withDefaults() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultAnalyticsTickInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultAnalyticsBatchSize
	}
	if c.BatchSize > maxAnalyticsBatchSize {
		c.BatchSize = maxAnalyticsBatchSize
	}
	return c
}

// NewDispatcher builds the analytics worker over the fixed source set.
func NewDispatcher(reader SourceReader, cursor CursorStore, sender Sender, cfg Config, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		reader:  reader,
		cursor:  cursor,
		sender:  sender,
		cfg:     cfg.withDefaults(),
		log:     log,
		sources: Sources(),
		now:     time.Now,
	}
}

// TickResult counts one pass. Zero values are the steady state.
type TickResult struct {
	Shipped int // tracked events sent to Amplitude
	Skipped int // valid rows that are not tracked product events
	Poison  int // rows whose payload could not be decoded
	Retry   int // sources whose batch failed to ship (cursor left in place)
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick — a transient error must not kill the process.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.TickInterval)
	defer t.Stop()
	d.log.Info("analytics dispatcher started",
		slog.Duration("tick", d.cfg.TickInterval),
		slog.Int("batch", d.cfg.BatchSize))
	for {
		select {
		case <-ctx.Done():
			d.log.Info("analytics dispatcher stopped")
			return nil
		case <-t.C:
			res, err := d.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				d.log.Error("analytics dispatcher tick failed", slog.String("error", err.Error()))
				continue
			}
			if res != (TickResult{}) {
				d.log.Info("analytics dispatcher tick",
					slog.Int("shipped", res.Shipped), slog.Int("skipped", res.Skipped),
					slog.Int("poison", res.Poison), slog.Int("retry", res.Retry))
			}
		}
	}
}

// Tick runs one pass over every source. Exported so tests can drive it directly.
func (d *Dispatcher) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult
	for _, src := range d.sources {
		if err := d.tickSource(ctx, src, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (d *Dispatcher) tickSource(ctx context.Context, src SourceName, res *TickResult) error {
	cur, err := d.cursor.Get(ctx, src)
	if err != nil {
		return fmt.Errorf("get analytics cursor %s: %w", src, err)
	}
	rows, err := d.reader.ListSince(ctx, src, cur, d.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("list %s since cursor: %w", src, err)
	}
	if len(rows) == 0 {
		return nil
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		ev, tracked, mErr := mapRow(src, row)
		if mErr != nil {
			// Undecodable payload can never succeed; skip it (the cursor will
			// still pass over it after a successful ship) so it never blocks.
			d.log.Error("analytics dispatcher: undecodable outbox payload, skipping",
				slog.String("source", string(src)),
				slog.String("row_id", row.ID.String()),
				slog.String("error", mErr.Error()))
			res.Poison++
			continue
		}
		if !tracked {
			res.Skipped++
			continue
		}
		events = append(events, ev)
	}

	if len(events) > 0 {
		if err := d.sender.Send(ctx, events); err != nil {
			// Transient failure: leave the cursor untouched so the whole batch
			// is reshipped next tick (Amplitude dedupes on device_id+insert_id).
			d.log.Error("analytics dispatcher: ship failed, batch left for retry",
				slog.String("source", string(src)),
				slog.Int("events", len(events)),
				slog.String("error", err.Error()))
			res.Retry++
			return nil
		}
		res.Shipped += len(events)
	}

	// Advance past the whole batch (tracked, skipped and poison rows alike).
	last := rows[len(rows)-1]
	if err := d.cursor.Save(ctx, src, Cursor{CreatedAt: last.CreatedAt, ID: last.ID}); err != nil {
		return fmt.Errorf("save analytics cursor %s: %w", src, err)
	}
	return nil
}
