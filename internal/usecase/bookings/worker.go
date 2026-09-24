package bookings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

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
	deposits    DepositSettler
	reminders   domain.BookingReminderRepository
	// release and holds drive the pre-order gate (owner decisions 2026-09-24);
	// both nil = the previous behaviour.
	release domain.BookingReleaseRepository
	holds   PreorderHoldChecker
}

// PreorderHoldChecker reports whether a booking's pre-order is currently a live
// HOLD (authorized, not yet captured). Silence of the venue on such a booking is
// a cancellation, never an auto-confirmation.
type PreorderHoldChecker interface {
	HasPreorderHold(ctx context.Context, bookingID uuid.UUID) (bool, error)
}

// WithWorkerPreorderGate wires the hidden-booking payment deadline (release) and
// the held-booking venue deadline (holds).
func WithWorkerPreorderGate(release domain.BookingReleaseRepository, holds PreorderHoldChecker) WorkerOption {
	return func(w *Worker) { w.release, w.holds = release, holds }
}

// WorkerOption configures optional worker dependencies without breaking the
// constructor's existing positional callers (tests pass none).
type WorkerOption func(*Worker)

// WithWorkerDepositSettler wires the deposit settlement the worker runs for the
// bookings it closes: a no-show forfeits the held deposit to the venue, a
// venue-never-responded abandonment releases it back to the guest. Left nil in
// tests / when payments are disabled (no settlement then).
func WithWorkerDepositSettler(d DepositSettler) WorkerOption {
	return func(w *Worker) { w.deposits = d }
}

// WithGuestReminders enables the pre-visit guest reminder pass. Left nil in
// tests that do not exercise it, and in any deployment that has not run
// migration 0049 — with a nil repository the pass is simply not run, exactly
// like the deposit settler.
func WithGuestReminders(r domain.BookingReminderRepository) WorkerOption {
	return func(w *Worker) { w.reminders = r }
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
	// ReminderLead is how long before starts_at the guest gets their pre-visit
	// reminder. The old Supabase system sent two (60 and 30 minutes); this one
	// sends exactly one per booking. env: WORKER_GUEST_REMINDER_LEAD
	ReminderLead time.Duration
	// BatchSize caps how many bookings one pass claims per stage.
	BatchSize int
	// AwaitPaymentTTL is how long a booking hidden behind an unpaid pre-order
	// lives before the system cancels it (D_pay, min age).
	// env: PAYMENTS_PREORDER_AWAIT_PAYMENT_TTL
	AwaitPaymentTTL time.Duration
	// ConfirmMax caps the venue's answer time for a booking with a held
	// pre-order (D_venue). env: PAYMENTS_PREORDER_CONFIRM_MAX
	ConfirmMax time.Duration
}

const (
	defaultTickInterval = time.Minute
	defaultNoShowGrace  = 30 * time.Minute
	defaultReminderLead = 60 * time.Minute
	defaultBatchSize    = 100

	DefaultAwaitPaymentTTL = 30 * time.Minute
	DefaultConfirmMax      = 24 * time.Hour
	// MaxConfirmMax is the hard ceiling checked at startup (spec criterion 23).
	MaxConfirmMax = 72 * time.Hour
	// minVenueAnswer: the venue always gets at least this long after release.
	minVenueAnswer = 15 * time.Minute
)

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.NoShowGrace < 0 {
		c.NoShowGrace = defaultNoShowGrace
	}
	if c.ReminderLead <= 0 {
		c.ReminderLead = defaultReminderLead
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.AwaitPaymentTTL <= 0 {
		c.AwaitPaymentTTL = DefaultAwaitPaymentTTL
	}
	if c.ConfirmMax <= 0 {
		c.ConfirmMax = DefaultConfirmMax
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
	opts ...WorkerOption,
) *Worker {
	w := &Worker{
		bookings: bookingsRepo, history: history, outbox: outbox,
		restaurants: restaurants, tx: tx,
		cfg: cfg.withDefaults(), wcfg: wcfg.withDefaults(),
		log: log, now: time.Now,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// TickResult counts what one pass did. Zero values are the normal steady state.
type TickResult struct {
	Confirmed int // pending/waitlist auto-confirmed
	Escalated int // confirm SLA breached, venue has auto_confirm off
	Abandoned int // pending/waitlist the venue never answered → cancelled
	Unpaid    int // hidden pre-order booking never paid → cancelled
	NoAnswer  int // held pre-order booking the venue never answered → cancelled
	Completed int // arrived → completed
	NoShow    int // confirmed → no_show
	Reminded  int // pre-visit guest reminders emitted
	Skipped   int // claimed but not actionable (SLA not reached, illegal transition)
}

func (r TickResult) attrs() []any {
	return []any{
		slog.Int("confirmed", r.Confirmed), slog.Int("escalated", r.Escalated),
		slog.Int("abandoned", r.Abandoned), slog.Int("completed", r.Completed),
		slog.Int("no_show", r.NoShow), slog.Int("reminded", r.Reminded),
		slog.Int("skipped", r.Skipped),
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
	// Deposit settlements to run AFTER the passes commit: each makes an external
	// acquirer call, which must never run inside the pass transaction (it holds
	// the ClaimDue row locks). Collected here, settled by settleDeposits below.
	var abandonedIDs, noShowIDs, unpaidIDs, noAnswerIDs []uuid.UUID
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		ids, n, err := w.processUnpaidHidden(ctx, now)
		unpaidIDs, res.Unpaid = ids, n
		return err
	}); err != nil {
		return res, fmt.Errorf("unpaid preorder pass: %w", err)
	}
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		ids, r, err := w.processAbandoned(ctx, now)
		abandonedIDs = ids
		res.Abandoned, res.Skipped = r.Abandoned, r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("abandoned pass: %w", err)
	}
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, ids, err := w.processConfirmSLA(ctx, now)
		noAnswerIDs = ids
		res.Confirmed, res.Escalated, res.NoAnswer = r.Confirmed, r.Escalated, r.NoAnswer
		res.Skipped += r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("confirm sla pass: %w", err)
	}
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		ids, r, err := w.processExpired(ctx, now)
		noShowIDs = ids
		res.Completed, res.NoShow = r.Completed, r.NoShow
		res.Skipped += r.Skipped
		return err
	}); err != nil {
		return res, fmt.Errorf("expiry pass: %w", err)
	}
	// Reminders run LAST: the passes above may have just cancelled or closed a
	// booking, and a guest must never be reminded about a visit that stopped
	// existing a millisecond earlier.
	if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := w.processReminders(ctx, now)
		res.Reminded = n
		return err
	}); err != nil {
		return res, fmt.Errorf("reminder pass: %w", err)
	}
	// A no-show forfeits the held deposit to the venue; a venue-never-responded
	// abandonment releases it to the guest. Outside every transaction, and never
	// fatal: a settlement error is logged and left for the reconciliation worker.
	w.settleDeposits(ctx, noShowIDs, domain.RefundTriggerNoShow)
	w.settleDeposits(ctx, abandonedIDs, domain.RefundTriggerVenueCancel)
	w.settleDeposits(ctx, unpaidIDs, domain.RefundTriggerVenueCancel)
	w.settleDeposits(ctx, noAnswerIDs, domain.RefundTriggerVenueCancel)
	return res, nil
}

// settleDeposits runs the held-deposit money decision for a set of just-closed
// bookings, outside any transaction. A booking with no deposit is a cheap
// no-op inside the settler; an error never fails the tick.
func (w *Worker) settleDeposits(ctx context.Context, bookingIDs []uuid.UUID, trigger domain.RefundTrigger) {
	if w.deposits == nil {
		return
	}
	for _, id := range bookingIDs {
		// cancelledAt is nil: a no-show is decided at the visit window's lapse
		// (the settler treats no-show as a late forfeit regardless of timing),
		// and a venue-side release does not consult it either.
		if err := w.deposits.SettleDepositOnCancel(ctx, id, trigger, nil); err != nil {
			w.log.Error("booking worker deposit settlement failed",
				slog.String("booking_id", id.String()),
				slog.String("trigger", string(trigger)),
				slog.String("error", err.Error()))
		}
	}
}

// processConfirmSLA handles bookings the venue has not answered in time.
//
// The SLA is per venue, so it cannot be pushed into the WHERE clause without
// joining restaurants; candidates are claimed with a "now" cutoff and each one
// is re-checked against its venue's resolved policy. That is safe: the set of
// unanswered bookings is small by construction (auto-confirm is on by default
// and drains it every tick) and BatchSize bounds the pass either way.
func (w *Worker) processConfirmSLA(ctx context.Context, now time.Time) (TickResult, []uuid.UUID, error) {
	var res TickResult
	var noAnswer []uuid.UUID
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingPending, domain.BookingWaitlist},
		domain.ClaimByCreatedAt, now, w.wcfg.BatchSize)
	if err != nil {
		return res, nil, err
	}
	for i := range due {
		b := due[i]
		if b.ReleasedToVenueAt == nil {
			// Hidden behind an unpaid pre-order: the venue has not even been
			// told, so its SLA has not started and nothing may auto-confirm it.
			res.Skipped++
			continue
		}
		rest, err := w.restaurants.GetByID(ctx, b.RestaurantID)
		if err != nil {
			return res, nil, fmt.Errorf("load restaurant %s: %w", b.RestaurantID, err)
		}
		policy := resolvePolicy(rest.Restaurant, w.cfg)
		if now.Before(b.ReleasedToVenueAt.Add(policy.ConfirmSLA)) {
			res.Skipped++
			continue
		}
		if w.holds != nil && b.Status == domain.BookingPending {
			held, err := w.holds.HasPreorderHold(ctx, b.ID)
			if err != nil {
				return res, nil, err
			}
			if held {
				// Silence is never a confirmation when money is on hold: at
				// D_venue the booking is cancelled and the hold voided.
				if now.Before(w.venueDeadline(b, policy)) {
					res.Skipped++
					continue
				}
				code := domain.CancelReasonVenueNoAnswer
				ok, err := w.transition(ctx, &b, domain.BookingCancelled, now, code,
					func(b *domain.Booking, at time.Time) {
						by := domain.CancelledBySystem
						b.CancelledBy, b.CancelledAt, b.CancellationReasonCode = &by, &at, &code
					})
				if err != nil {
					return res, nil, err
				}
				if !ok {
					res.Skipped++
					continue
				}
				res.NoAnswer++
				noAnswer = append(noAnswer, b.ID)
				continue
			}
		}
		if !policy.AutoConfirm {
			// Escalate at most once per booking; the venue keeps ownership of
			// the decision and the booking stays pending.
			exists, err := w.outbox.ExistsForBooking(ctx, b.ID, domain.EventBookingEscalated)
			if err != nil {
				return res, nil, err
			}
			if exists {
				res.Skipped++
				continue
			}
			if err := publish(ctx, w.outbox, &b, domain.EventBookingEscalated, now); err != nil {
				return res, nil, err
			}
			res.Escalated++
			continue
		}
		ok, err := w.transition(ctx, &b, domain.BookingConfirmed, now, "confirm sla elapsed, venue auto-confirm", nil)
		if err != nil {
			return res, nil, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		res.Confirmed++
	}
	return res, noAnswer, nil
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
func (w *Worker) processAbandoned(ctx context.Context, now time.Time) ([]uuid.UUID, TickResult, error) {
	var res TickResult
	var settled []uuid.UUID
	cutoff := now.Add(-w.wcfg.NoShowGrace)
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingPending, domain.BookingWaitlist},
		domain.ClaimByEndsAt, cutoff, w.wcfg.BatchSize)
	if err != nil {
		return nil, res, err
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
			return nil, res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		res.Abandoned++
		settled = append(settled, b.ID)
	}
	return settled, res, nil
}

// processExpired closes bookings whose visit window is over: arrived guests are
// completed, guests never marked as arrived become no_show.
func (w *Worker) processExpired(ctx context.Context, now time.Time) ([]uuid.UUID, TickResult, error) {
	var res TickResult
	var noShowIDs []uuid.UUID
	cutoff := now.Add(-w.wcfg.NoShowGrace)
	due, err := w.bookings.ClaimDue(ctx,
		[]domain.BookingStatus{domain.BookingArrived, domain.BookingConfirmed},
		domain.ClaimByEndsAt, cutoff, w.wcfg.BatchSize)
	if err != nil {
		return nil, res, err
	}
	for i := range due {
		b := due[i]
		to, reason := domain.BookingCompleted, "visit window ended"
		if b.Status == domain.BookingConfirmed {
			to, reason = domain.BookingNoShow, "guest was never marked as arrived"
		}
		ok, err := w.transition(ctx, &b, to, now, reason, nil)
		if err != nil {
			return nil, res, err
		}
		if !ok {
			res.Skipped++
			continue
		}
		if to == domain.BookingCompleted {
			res.Completed++
		} else {
			res.NoShow++
			noShowIDs = append(noShowIDs, b.ID)
		}
	}
	return noShowIDs, res, nil
}

// processReminders emits the pre-visit reminder for bookings whose visit is
// within ReminderLead. It is the one pass that changes NO status: it stamps
// bookings.guest_reminder_sent_at and writes one booking.reminder outbox event,
// which the notification dispatcher then delivers to the guest's devices — the
// same delivery path every other booking event takes.
//
// Idempotency, in one place: MarkReminderSent is a conditional UPDATE
// (guest_reminder_sent_at IS NULL AND status is still live). It runs in the same
// transaction as the outbox insert, so:
//
//   - a second tick finds the marker set, gets false, and emits nothing;
//   - a crash between the stamp and the event rolls BOTH back, so the reminder
//     is re-emitted on the next tick rather than silently lost;
//   - a booking cancelled between the claim and the stamp fails the status
//     predicate and is skipped (the claim's row lock already prevents this, but
//     the predicate holds even if the pass is ever run without one).
//
// A nil reminders repository (worker built without WithGuestReminders) turns the
// pass into a no-op — the same discipline as the optional deposit settler.
func (w *Worker) processReminders(ctx context.Context, now time.Time) (int, error) {
	if w.reminders == nil {
		return 0, nil
	}
	due, err := w.reminders.ClaimDueReminders(ctx, now, now.Add(w.wcfg.ReminderLead), w.wcfg.BatchSize)
	if err != nil {
		return 0, err
	}
	sent := 0
	for i := range due {
		b := due[i]
		ok, err := w.reminders.MarkReminderSent(ctx, b.ID, now)
		if err != nil {
			return sent, err
		}
		if !ok {
			// Already reminded, or no longer live. Not our booking to announce.
			continue
		}
		if err := publish(ctx, w.outbox, &b, domain.EventBookingReminder, now); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
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

// venueDeadline is D_venue: min(released + venue SLA, released + ConfirmMax,
// starts_at), but never earlier than released + 15 minutes.
func (w *Worker) venueDeadline(b domain.Booking, policy domain.BookingPolicy) time.Time {
	rel := *b.ReleasedToVenueAt
	d := rel.Add(policy.ConfirmSLA)
	if m := rel.Add(w.wcfg.ConfirmMax); m.Before(d) {
		d = m
	}
	if b.StartsAt.Before(d) {
		d = b.StartsAt
	}
	if floor := rel.Add(minVenueAnswer); d.Before(floor) {
		d = floor
	}
	return d
}

// processUnpaidHidden cancels bookings that stayed hidden behind an unpaid
// pre-order past D_pay (created + AwaitPaymentTTL, and no live payment link):
// the guest closed the page, the card was declined or the link expired. The
// table is freed by the status trigger; the guest is told through the ordinary
// cancelled event. Returns the cancelled ids so any leftover payment is settled
// after the transaction.
func (w *Worker) processUnpaidHidden(ctx context.Context, now time.Time) ([]uuid.UUID, int, error) {
	if w.release == nil {
		return nil, 0, nil
	}
	due, err := w.release.ClaimUnpaidHidden(ctx, now.Add(-w.wcfg.AwaitPaymentTTL), w.wcfg.BatchSize)
	if err != nil {
		return nil, 0, err
	}
	var ids []uuid.UUID
	code := domain.CancelReasonPreorderPaymentNotCompleted
	for i := range due {
		b := due[i]
		ok, err := w.transition(ctx, &b, domain.BookingCancelled, now, code,
			func(b *domain.Booking, at time.Time) {
				by := domain.CancelledBySystem
				b.CancelledBy, b.CancelledAt, b.CancellationReasonCode = &by, &at, &code
			})
		if err != nil {
			return nil, 0, err
		}
		if ok {
			ids = append(ids, b.ID)
		}
	}
	return ids, len(ids), nil
}
