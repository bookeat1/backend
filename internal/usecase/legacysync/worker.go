package legacysync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"backend-core/internal/domain"
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
	// Entities is the ALLOWLIST of entities this sync is permitted to write.
	// Empty means DefaultEntities. env: LEGACY_SYNC_ENTITIES.
	//
	// The sync no longer moves the catalog. Since the mobile apps went live the
	// new database — and the admin panel on top of it — OWNS venues, tables,
	// menus and schedules; the old system stayed behind as the engine of the
	// web site, which still produces bookings. A pass that kept importing
	// venues did not "top the data up", it silently reverted whatever the owner
	// had edited in the cabinet (see the 2026-08-27 rename complaint), and
	// nothing anywhere reported an error, because from the sync's point of view
	// the write succeeded.
	//
	// This is a list rather than a code deletion on purpose: a one-off import
	// of some other entity may still be wanted, and it must be a deliberate,
	// visible act (one env var for one run) instead of a code change.
	Entities []string
}

// DefaultEntities is what the sync writes when LEGACY_SYNC_ENTITIES is unset:
// bookings, and nothing else.
//
// Why bookings alone:
//   - the web site still books against the old base, and those reservations
//     have to reach the venue's cabinet — this is the entire remaining reason
//     the sync exists;
//   - venues / tables / menu categories / menu items / working hours are ours
//     now, and re-importing them overwrites live cabinet edits;
//   - EntityBookingTables is off as well, even though it is part of "bookings":
//     a table assignment points at a restaurant_tables id, and those ids stop
//     arriving the moment EntityTables is off — every such row would park for a
//     parent that is never coming. A booking without a table hold still shows
//     up in the cabinet; a permanently parked row would just wedge the cursor;
//   - USERS ARE NOT IN THIS LIST BECAUSE THIS SYNC HAS NO USERS ENTITY AT ALL
//     (there is no Source.Users, no Sink.UpsertUser and no "users" cursor). A
//     legacy booking made by a web account keeps its user_id only if a user
//     with that id already exists here, otherwise the column is left NULL and
//     the booking arrives as a guest booking with its name/phone — which is
//     also how the guest later reclaims it by verifying that phone
//     (AttachOrphanedByPhone). Importing accounts would be new work, not a
//     switch to flip.
var DefaultEntities = []string{EntityBookings}

// KnownEntities lists every entity name Entities may contain. A name outside
// this set is a typo, and a typo must not read as "that entity is off".
func KnownEntities() []string {
	return []string{
		EntityRestaurants, EntityTables, EntityMenuCategories, EntityMenuItems,
		EntityBookings, EntityBookingTables, EntityWorkingHours,
	}
}

// ValidateEntities rejects an unknown entity name. Called at wiring time so a
// mistyped LEGACY_SYNC_ENTITIES fails the worker's startup loudly instead of
// quietly syncing less than the operator asked for.
func ValidateEntities(names []string) error {
	known := make(map[string]bool, len(KnownEntities()))
	for _, k := range KnownEntities() {
		known[k] = true
	}
	for _, n := range names {
		if !known[n] {
			return fmt.Errorf("legacy sync: unknown entity %q (known: %s)",
				n, strings.Join(KnownEntities(), ", "))
		}
	}
	return nil
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
	if len(c.Entities) == 0 {
		c.Entities = DefaultEntities
	}
	return c
}

// enabled reports whether entity is in the configured allowlist.
func (w *Worker) enabled(entity string) bool {
	for _, e := range w.cfg.Entities {
		if e == entity {
			return true
		}
	}
	return false
}

// Worker periodically pulls changed rows from the old system and upserts them
// into the new one. It is safe to run idle: with no source changes every tick
// is a cheap no-op, and it is only ever started when LEGACY_DB_URL is set.
type Worker struct {
	source Source
	sink   Sink
	tx     domain.TxManager
	cfg    Config
	log    *slog.Logger
}

// NewWorker builds the sync worker. source must be a READ-ONLY view of the old
// DB; sink writes the new DB. tx wraps the working-hours fill of ONE venue, so
// the "does the venue own these hours" check and the write it authorises cannot
// be split by a concurrent admin edit.
func NewWorker(source Source, sink Sink, tx domain.TxManager, cfg Config, log *slog.Logger) *Worker {
	return &Worker{source: source, sink: sink, tx: tx, cfg: cfg.withDefaults(), log: log}
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
		slog.Int("batch", w.cfg.BatchSize),
		// The allowlist is the single most consequential setting here: it is
		// the difference between "import the bookings the web site takes" and
		// "revert everything the cabinet edited". It belongs in the startup
		// line, where an operator turning the sync on for one run will see it.
		slog.String("entities", strings.Join(w.cfg.Entities, ",")))
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
	// Order is FK-safe (parents first) and stays that way whatever the
	// allowlist keeps: filtering preserves it.
	steps := []struct {
		entity string
		run    func(context.Context) (EntityResult, error)
	}{
		{EntityRestaurants, w.syncRestaurants},
		{EntityTables, w.syncTables},
		{EntityMenuCategories, w.syncMenuCategories},
		{EntityMenuItems, w.syncMenuItems},
		{EntityBookings, w.syncBookings},
		{EntityBookingTables, w.syncBookingTables},
	}
	var firstErr error
	for _, step := range steps {
		if !w.enabled(step.entity) {
			continue
		}
		res, err := step.run(ctx)
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
	// Working hours run last and are NOT part of the steps loop: they are not
	// cursored, they are derived from the restaurants rows the pass above has
	// just written, and they report their own counters (see workinghours.go).
	// They are gated by the same allowlist — the pass reads the venues' legacy
	// free text, so leaving it on while venues are off would keep rewriting a
	// schedule from a source we no longer trust.
	if w.enabled(EntityWorkingHours) {
		hours, err := w.syncWorkingHours(ctx)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		w.logWorkingHours(hours)
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
			// The row id is in the line on purpose. A park is retried forever
			// and holds the cursor where it is, so a parent that is never
			// coming (a venue that only ever existed in the old system, now
			// that venues are not imported) shows up as the same warning every
			// tick — an operator needs to know WHICH row, not just that one
			// exists.
			log.Warn("legacy sync row parked (parent not synced yet)",
				slog.String("entity", entity),
				slog.String("row_id", key(row).ID.String()))
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
