package legacysync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"backend-core/internal/logging"
)

// Config is the sync worker's scheduling and safety configuration, same
// env-driven convention as the other background workers.
type Config struct {
	// TickInterval is the pause between two passes. env: LEGACY_SYNC_TICK_INTERVAL
	TickInterval time.Duration
	// BatchSize caps how many rows one pass pulls per entity. env: LEGACY_SYNC_BATCH_SIZE
	BatchSize int
	// DefaultDuration is added to a booking's single stored time to derive the
	// required ends_at / slot end. env: BOOKING_DEFAULT_DURATION_MINUTES (shared).
	DefaultDuration time.Duration
}

const (
	defaultTickInterval    = time.Minute
	defaultBatchSize       = 500
	defaultBookingDuration = 90 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.DefaultDuration <= 0 {
		c.DefaultDuration = defaultBookingDuration
	}
	return c
}

// Worker periodically pulls changed rows from the old system and upserts them
// into the new one. It is safe to run idle: with no source changes every tick
// is a cheap no-op, and it is only ever started when LEGACY_DB_URL is set.
type Worker struct {
	source Source
	sink   Sink
	cfg    Config
	log    *slog.Logger
}

// NewWorker builds the sync worker. source must be a READ-ONLY view of the old
// DB; sink writes the new DB.
func NewWorker(source Source, sink Sink, cfg Config, log *slog.Logger) *Worker {
	return &Worker{source: source, sink: sink, cfg: cfg.withDefaults(), log: log}
}

// EntityResult counts what one entity's pass did.
type EntityResult struct {
	Entity  string
	Fetched int
	Written int
	Parked  int
	Skipped int
}

func (r EntityResult) empty() bool {
	return r.Fetched == 0 && r.Written == 0 && r.Parked == 0 && r.Skipped == 0
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick, never fatal — same contract as the other workers' Run.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()
	w.log.Info("legacy sync started",
		slog.Duration("tick", w.cfg.TickInterval),
		slog.Int("batch", w.cfg.BatchSize))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("legacy sync stopped")
			return nil
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				w.log.Error("legacy sync tick failed", slog.String("error", err.Error()))
			}
		}
	}
}

// Tick runs one full pass over every entity, in FK-safe order so a child's
// parent is synced before the child in the same pass. A failure on one entity
// is returned (and logged by Run) but does not corrupt state: each entity
// advances its own cursor only over the rows it actually wrote.
func (w *Worker) Tick(ctx context.Context) error {
	steps := []func(context.Context) (EntityResult, error){
		w.syncRestaurants,
		w.syncTables,
		w.syncMenuCategories,
		w.syncMenuItems,
		w.syncBookings,
		w.syncBookingTables,
	}
	var firstErr error
	for _, step := range steps {
		res, err := step(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.log.Error("legacy sync entity failed",
				slog.String("entity", res.Entity), slog.String("error", err.Error()))
			continue
		}
		if !res.empty() {
			w.log.Info(logging.EventLegacySyncTick,
				slog.String("entity", res.Entity),
				slog.Int("fetched", res.Fetched),
				slog.Int("written", res.Written),
				slog.Int("parked", res.Parked),
				slog.Int("skipped", res.Skipped))
		}
	}
	return firstErr
}

// syncEntity is the shared per-entity loop: read a bounded, ordered batch after
// the stored cursor, upsert each row, and advance the cursor over the longest
// contiguous run of rows that were Written or Skipped. A Parked row (parent not
// synced yet) stops the cursor advancing past it — it is retried next tick and
// never lost — but later rows in the batch are still attempted (idempotent, a
// harmless head start). The cursor is persisted once, only after the batch has
// been processed.
func syncEntity[T any](
	ctx context.Context,
	entity string,
	sink Sink,
	fetch func(context.Context, Cursor, int) ([]T, error),
	key func(T) Cursor,
	upsert func(context.Context, T) (Outcome, error),
	batch int,
	log *slog.Logger,
) (EntityResult, error) {
	res := EntityResult{Entity: entity}
	cur, err := sink.GetCursor(ctx, entity)
	if err != nil {
		return res, err
	}
	rows, err := fetch(ctx, cur, batch)
	if err != nil {
		return res, err
	}
	res.Fetched = len(rows)

	watermark := cur
	contiguous := true
	for _, row := range rows {
		outcome, err := upsert(ctx, row)
		if err != nil {
			// A real infrastructure error: stop, persist whatever prefix we
			// safely advanced, and let the tick retry the rest next time.
			if werr := advanceCursor(ctx, sink, entity, cur, watermark); werr != nil {
				return res, werr
			}
			return res, err
		}
		switch outcome {
		case Written:
			res.Written++
			if contiguous {
				watermark = key(row)
			}
		case Skipped:
			res.Skipped++
			if contiguous {
				watermark = key(row)
			}
		case Parked:
			res.Parked++
			contiguous = false
			log.Warn("legacy sync row parked (parent not synced yet)",
				slog.String("entity", entity))
		}
	}
	if err := advanceCursor(ctx, sink, entity, cur, watermark); err != nil {
		return res, err
	}
	return res, nil
}

func advanceCursor(ctx context.Context, sink Sink, entity string, from, to Cursor) error {
	if to == from {
		return nil
	}
	return sink.SetCursor(ctx, entity, to)
}

func (w *Worker) syncRestaurants(ctx context.Context) (EntityResult, error) {
	return syncEntity(ctx, EntityRestaurants, w.sink,
		w.source.Restaurants, Restaurant.Cursor, w.sink.UpsertRestaurant,
		w.cfg.BatchSize, w.log)
}

func (w *Worker) syncTables(ctx context.Context) (EntityResult, error) {
	return syncEntity(ctx, EntityTables, w.sink,
		w.source.Tables, Table.Cursor, w.sink.UpsertTable,
		w.cfg.BatchSize, w.log)
}

func (w *Worker) syncMenuCategories(ctx context.Context) (EntityResult, error) {
	return syncEntity(ctx, EntityMenuCategories, w.sink,
		w.source.MenuCategories, MenuCategory.Cursor, w.sink.UpsertMenuCategory,
		w.cfg.BatchSize, w.log)
}

func (w *Worker) syncMenuItems(ctx context.Context) (EntityResult, error) {
	return syncEntity(ctx, EntityMenuItems, w.sink,
		w.source.MenuItems, MenuItem.Cursor, w.sink.UpsertMenuItem,
		w.cfg.BatchSize, w.log)
}

// syncBookings wraps the generic loop with the raw->new mapping. A booking's
// ends_at is derived from its restaurant's booking_duration_minutes (loaded once
// per pass; restaurants sync before bookings so the value is present), falling
// back to the env default. A row whose status is unrecognized or whose guest
// count is non-positive is dropped as Skipped (logged), never coerced.
func (w *Worker) syncBookings(ctx context.Context) (EntityResult, error) {
	durations, err := w.sink.RestaurantDurations(ctx)
	if err != nil {
		return EntityResult{Entity: EntityBookings}, err
	}
	fetch := func(ctx context.Context, cur Cursor, limit int) ([]LegacyBooking, error) {
		return w.source.Bookings(ctx, cur, limit)
	}
	upsert := func(ctx context.Context, l LegacyBooking) (Outcome, error) {
		dur := resolveDuration(durations, l.RestaurantID, w.cfg.DefaultDuration)
		b, ok := mapBooking(l, dur)
		if !ok {
			w.log.Warn("legacy sync booking skipped (bad status or guest count)",
				slog.String("booking_id", l.ID.String()),
				slog.String("status", l.Status),
				slog.Int("guests", l.Guests))
			return Skipped, nil
		}
		return w.sink.UpsertBooking(ctx, b)
	}
	return syncEntity(ctx, EntityBookings, w.sink,
		fetch, LegacyBooking.Cursor, upsert, w.cfg.BatchSize, w.log)
}

// syncBookingTables uses the generic loop like every other entity: the Source
// now paginates the UNION with genuine keyset pagination on (updated_at,
// sort_id) — sort_id (bt.id or booking id) is unique per row and computable in
// SQL, so a batch boundary is exhaustive and gap-free even when many rows share
// one updated_at. The slot end uses the same per-restaurant resolved duration as
// the booking's ends_at.
func (w *Worker) syncBookingTables(ctx context.Context) (EntityResult, error) {
	durations, err := w.sink.RestaurantDurations(ctx)
	if err != nil {
		return EntityResult{Entity: EntityBookingTables}, err
	}
	fetch := func(ctx context.Context, cur Cursor, limit int) ([]LegacyBookingTable, error) {
		return w.source.BookingTables(ctx, cur, limit)
	}
	upsert := func(ctx context.Context, l LegacyBookingTable) (Outcome, error) {
		dur := resolveDuration(durations, l.RestaurantID, w.cfg.DefaultDuration)
		return w.sink.UpsertBookingTable(ctx, mapBookingTable(l, dur))
	}
	return syncEntity(ctx, EntityBookingTables, w.sink,
		fetch, LegacyBookingTable.Cursor, upsert, w.cfg.BatchSize, w.log)
}
