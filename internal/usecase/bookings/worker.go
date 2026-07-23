package bookings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend-core/internal/domain"
)

// Worker is the background booking janitor (spec §5). Once per tick it:
//
//   - closes bookings the venue never answered: pending / waitlist whose visit
//     window ended more than NoShowGrace ago become cancelled, attributed to
//     the system. Without this stage such a booking is immortal — no_show is
//     reachable only from confirmed, so nothing else can ever move it;
//   - takes bookings still waiting for the venue (pending / waitlist) whose
//     confirm SLA has expired: auto-confirms them when the venue has
//     auto_confirm on, otherwise records one escalation event for the venue;
//   - closes bookings whose visit window ended more than NoShowGrace ago:
//     arrived → completed, confirmed → no_show (the guest was never marked as
//     arrived).
//
// The abandoned stage runs FIRST on purpose: a request whose visit time has
// already come and gone must not be auto-confirmed into a booking nobody can
// honour, only to be marked as the guest's no-show one stage later.
//
// Every selection goes through the repository's ClaimDue, which uses
// SELECT ... FOR UPDATE SKIP LOCKED inside a transaction, so several worker
// instances may run in parallel without touching the same booking. Each
// transition writes bookings + booking_status_history + booking_outbox in one
// transaction, through the same recordTransition path the HTTP layer uses.
type Worker struct {
	bookings    domain.BookingRepository
	history     domain.BookingStatusHistoryRepository
	outbox      domain.BookingOutboxRepository
	restaurants restaurantReader
	tx          domain.TxManager
	cfg         Config
	wcfg        WorkerConfig
	log         *slog.Logger
	now         func() time.Time // injectable clock for tests
}

// WorkerConfig is the worker's own scheduling configuration. The per-booking
// policy (confirm SLA, auto-confirm) is NOT here — it is resolved per venue
// from Config plus the restaurant's overrides.
type WorkerConfig struct {
	// TickInterval is the pause between two passes. env: WORKER_TICK_INTERVAL
	TickInterval time.Duration
	// NoShowGrace is how long after ends_at a booking is left alone before it
	// is closed as completed / no_show. env: WORKER_NO_SHOW_GRACE
	NoShowGrace time.Duration
	// BatchSize caps how many bookings one pass claims per stage.
	BatchSize int
}

const (
	defaultTickInterval = time.Minute
	defaultNoShowGrace  = 30 * time.Minute
	defaultBatchSize    = 100
)

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.NoShowGrace < 0 {
		c.NoShowGrace = defaultNoShowGrace
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	return c
}

// NewWorker constructs the background booking worker.
func NewWorker(
	bookingsRepo domain.BookingRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	restaurants restaurantReader,
	tx domain.TxManager,
	cfg Config,
	wcfg WorkerConfig,
	log *slog.Logger,
) *Worker {
	return &Worker{
		bookings: bookingsRepo, history: history, outbox: outbox,
		restaurants: restaurants, tx: tx,
		cfg: cfg.withDefaults(), wcfg: wcfg.withDefaults(),
		log: log, now: time.Now,
	}
}

// TickResult counts what one pass did. Zero values are the normal steady state.
type TickResult struct {
	Confirmed int // pending/waitlist auto-confirmed
	Escalated int // confirm SLA breached, venue has auto_confirm off
	Abandoned int // pending/waitlist the venue never answered → cancelled
	Completed int // arrived → completed
	NoShow    int // confirmed → no_show
	Skipped   int // claimed but not actionable (SLA not reached, illegal transition)
}

func (r TickResult) attrs() []any {
	return []any{
		slog.Int("confirmed", r.Confirmed), slog.Int("escalated", r.Escalated),
		slog.Int("abandoned", r.Abandoned), slog.Int("completed", r.Completed),
		slog.Int("no_show", r.NoShow), slog.Int("skipped", r.Skipped),
	}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick — a transient database error must not kill the process.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.wcfg.TickInterval)
	defer t.Stop()
	w.log.Info("booking worker started",
		slog.Duration("tick", w.wcfg.TickInterval),
		slog.Duration("no_show_grace", w.wcfg.NoShowGrace))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("booking worker stopped")
			return nil
		case <-t.C:
			res, err := w.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				w.log.Error("booking worker tick failed", slog.String("error", err.Error()))
				continue
			}
			if res != (TickResult{}) {
				w.log.Info("booking worker tick", res.attrs()...)
			}
		}
	}
}

// Tick runs one pass. Exported so it can be driven directly from tests and from
// a one-shot invocation.
func (w *Worker) Tick(ctx context.Context) (TickResult, error) {
	now := w.now()
	var res TickResult
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := w.processAbandoned(ctx, now)
		res.Abandoned, res.Skipped = r.Abandoned, r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("abandoned pass: %w", err)
	}
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := w.processConfirmSLA(ctx, now)
		res.Confirmed, res.Escalated = r.Confirmed, r.Escalated
		res.Skipped += r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("confirm sla pass: %w", err)
	}
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := w.processExpired(ctx, now)
		res.Completed, res.NoShow = r.Completed, r.NoShow
		res.Skipped += r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("expiry pass: %w", err)
	}
	return res, nil
}

// processConfirmSLA handles bookings the venue has not answered in time.
//
// The SLA is per venue, so it cannot be pushed into the WHERE clause without
// joining restaurants; candidates are claimed with a "now" cutoff and each one
// is re-checked against its venue's resolved policy. That is safe: the set of
// unanswered bookings is small by construction (auto-confirm is on by default
// and drains it every tick) and BatchSize bounds the pass either way.
func (w *Worker) processConfirmSLA(ctx context.Context, now time.Time) (TickResult, error) {
	var res TickResult
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingPending, domain.BookingWaitlist},
		domain.ClaimByCreatedAt, now, w.wcfg.BatchSize)
	if err != nil {
		return res, err
	}
	for i := range due {
		b := due[i]
		rest, err := w.restaurants.GetByID(ctx, b.RestaurantID)
		if err != nil {
			return res, fmt.Errorf("load restaurant %s: %w", b.RestaurantID, err)
		}
		policy := resolvePolicy(rest.Restaurant, w.cfg)
		if now.Before(b.CreatedAt.Add(policy.ConfirmSLA)) {
			res.Skipped++
			continue
		}
		if !policy.AutoConfirm {
			// Escalate at most once per booking; the venue keeps ownership of
			// the decision and the booking stays pending.
			exists, err := w.outbox.ExistsForBooking(ctx, b.ID, domain.EventBookingEscalated)
			if err != nil {
				return res, err
			}
			if exists {
				res.Skipped++
				continue
			}
			if err := publish(ctx, w.outbox, &b, domain.EventBookingEscalated, now); err != nil {
				return res, err
			}
			res.Escalated++
			continue
		}
		ok, err := w.transition(ctx, &b, domain.BookingConfirmed, now, "confirm sla elapsed, venue auto-confirm", nil)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		res.Confirmed++
	}
	return res, nil
}

// abandonedReason is written to booking_status_history and to the booking's
// cancellation_reason so the venue can see in its own list why the request
// closed itself.
const abandonedReason = "venue never responded"

// processAbandoned closes requests the venue simply never answered: pending or
// waitlist bookings whose visit window ended more than NoShowGrace ago.
//
// They cannot become no_show (that edge exists only from confirmed — a venue
// that never accepted the booking has no promise for the guest to break), and
// nobody is going to open the tablet weeks later to clear them by hand, so the
// worker cancels them as the system with an explicit reason. Cancelling also
// releases the table: booking_tables.active is driven by the status trigger,
// and a dead pending booking must not sit on a slot forever.
func (w *Worker) processAbandoned(ctx context.Context, now time.Time) (TickResult, error) {
	var res TickResult
	cutoff := now.Add(-w.wcfg.NoShowGrace)
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingPending, domain.BookingWaitlist},
		domain.ClaimByEndsAt, cutoff, w.wcfg.BatchSize)
	if err != nil {
		return res, err
	}
	reason := abandonedReason
	for i := range due {
		b := due[i]
		ok, err := w.transition(ctx, &b, domain.BookingCancelled, now, reason,
			func(b *domain.Booking, at time.Time) {
				by := domain.CancelledBySystem
				b.CancelledBy = &by
				b.CancelledAt = &at
				b.CancellationReason = &reason
			})
		if err != nil {
			return res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		res.Abandoned++
	}
	return res, nil
}

// processExpired closes bookings whose visit window is over: arrived guests are
// completed, guests never marked as arrived become no_show.
func (w *Worker) processExpired(ctx context.Context, now time.Time) (TickResult, error) {
	var res TickResult
	cutoff := now.Add(-w.wcfg.NoShowGrace)
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingArrived, domain.BookingConfirmed},
		domain.ClaimByEndsAt, cutoff, w.wcfg.BatchSize)
	if err != nil {
		return res, err
	}
	for i := range due {
		b := due[i]
		to, reason := domain.BookingCompleted, "visit window ended"
		if b.Status == domain.BookingConfirmed {
			to, reason = domain.BookingNoShow, "guest was never marked as arrived"
		}
		ok, err := w.transition(ctx, &b, to, now, reason, nil)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		if to == domain.BookingCompleted {
			res.Completed++
		} else {
			res.NoShow++
		}
	}
	return res, nil
}

// transition applies one system-driven status change: bookings UPDATE + history
// row + outbox event. It must be called inside the transaction that holds the
// row lock from ClaimDue. Returns false (without an error) when the transition
// is not legal from the booking's current status — the worker races with the
// venue's own actions and must not fail the whole pass over one stale row.
//
// apply is an optional hook for the metadata columns UpdateStatus does not
// own (cancelled_by, cancellation_reason). Mirroring statusUseCase.transition,
// that metadata is written first and the status last, because the DB trigger
// that syncs booking_tables.active fires on the status write.
func (w *Worker) transition(
	ctx context.Context,
	b *domain.Booking,
	to domain.BookingStatus,
	at time.Time,
	reason string,
	apply func(b *domain.Booking, at time.Time),
) (bool, error) {
	from := b.Status
	if err := domain.ValidateTransition(from, to); err != nil {
		w.log.Warn("booking worker skipped illegal transition",
			slog.String("booking_id", b.ID.String()),
			slog.String("from", string(from)), slog.String("to", string(to)))
		return false, nil
	}
	if apply != nil {
		apply(b, at)
		if err := w.bookings.Update(ctx, b); err != nil {
			return false, err
		}
	}
	if err := w.bookings.UpdateStatus(ctx, b.ID, to, at); err != nil {
		return false, err
	}
	b.Status = to
	if to == domain.BookingConfirmed {
		b.ConfirmedAt = &at
	}
	if err := recordTransition(ctx, w.history, w.outbox, b, &from, domain.ActorSystem, nil, &reason, at); err != nil {
		return false, err
	}
	return true, nil
}
