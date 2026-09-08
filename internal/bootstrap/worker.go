package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
)

// RunWorker starts the background booking worker, the payments reconciliation
// worker, the notification dispatcher and the guest-push receipt poller side by
// side, and blocks until SIGINT
// or SIGTERM. The current pass of each is allowed to finish: the signal cancels
// the shared context, each worker returns from its own Run between ticks, and
// the pool is closed only after all have stopped.
//
// The payments reconciler is started unconditionally, even with
// PAYMENTS_ENABLED=false and no acquirer credentials configured: with zero
// gateways in the registry and zero payments in the database, every tick is a
// cheap no-op (ClaimStale / ClaimExpiredHolds simply find nothing). Gating it
// on PAYMENTS_ENABLED would mean flipping that flag on later requires a
// worker redeploy just to start reconciling — building the reconciler to be
// safe when idle is what makes running it unconditionally the safer default.
func RunWorker(cfg Config, log *slog.Logger) error {
	db, err := NewDB(cfg.DB.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()

	reconciler, err := NewPaymentsReconciler(cfg, db, log)
	if err != nil {
		return fmt.Errorf("build payments reconciler: %w", err)
	}
	// The notification dispatcher drains the booking outbox and fans
	// "booking.created" out to the web-push channel. Started unconditionally,
	// same rationale as the reconciler: with no VAPID keys the web-push notifier
	// no-ops, and with no unpublished events every tick is a cheap no-op — so
	// enabling push later never needs a worker redeploy.
	dispatcher := NewNotificationDispatcher(cfg, db, log)

	// The payout reconciler resolves payouts stranded in `sent` (a lost/unknown
	// acquirer answer). Started unconditionally, same rationale as the payments
	// reconciler: it is safe-idle when the payout gateway is disabled (nil) or
	// no payout is stale — Tick returns immediately.
	payoutReconciler := NewPayoutReconciler(cfg, db, log)

	// The daily payout pass generates ONE payout per venue per VENUE-LOCAL day
	// for that venue's settled money. Started unconditionally, same safe-idle
	// rationale as the reconcilers, plus two of its own: with
	// PAYOUTS_DAILY_SEND_ENABLED unset it only creates pending payouts and
	// moves no money at all, and the once-per-venue-per-day guarantee is a
	// UNIQUE index, so a second worker instance or a restart mid-pass cannot
	// pay anyone twice.
	dailyPayouts := NewDailyPayoutRunner(cfg, db, log)

	// The pending-ticket sweeper releases seats held by pending event tickets
	// whose payment never completed (or was never created). Started
	// unconditionally, same safe-idle rationale as the reconciler: with no stale
	// pending tickets every pass finds nothing.
	ticketSweeper := NewTicketSweeper(cfg, db, log)

	// The recurring-event generator fills the Афиша from the venues' recurrence
	// rules for a rolling window ahead. Started unconditionally, same safe-idle
	// rationale as the reconcilers: no rules means an empty page, an already
	// materialised window means an insert that writes nothing. Running two
	// instances is safe by construction (unique index on the slot).
	recurrenceGenerator := NewEventRecurrenceGenerator(cfg, db, log)

	// The analytics dispatcher ships product events (booking/payment) to
	// Amplitude by re-reading the existing outboxes through its own cursor.
	// Started unconditionally, same rationale as the notification dispatcher:
	// with no AMPLITUDE_API_KEY the sender no-ops, and with no new outbox rows
	// every tick is a cheap read — so enabling analytics later never needs a
	// worker redeploy, only the env var.
	analyticsDispatcher := NewAnalyticsDispatcher(cfg, db, log)

	// The guest-push receipt worker closes the loop the send path cannot: a
	// provider ticket says "accepted", and only the receipt says what happened
	// on the device. It is nil (and simply never started) when no push provider
	// is configured — unlike the reconcilers, it has no work that exists
	// independently of a provider, because without one nothing is ever sent.
	pushReceipts := NewPushReceiptWorker(cfg, db, log)

	// The legacy one-way sync (old Supabase -> new DB) is started only when
	// LEGACY_DB_URL is set. When it is unset legacySync is nil and the loop is
	// simply never started — a clean no-op, same discipline as the other
	// optional workers. closeLegacy owns the separate read-only pool to the old
	// database and is called on shutdown.
	legacySync, closeLegacy, err := NewLegacySyncWorker(cfg, db, log)
	if err != nil {
		return fmt.Errorf("build legacy sync worker: %w", err)
	}
	if closeLegacy != nil {
		defer closeLegacy()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var bookingErr, paymentsErr, notifyErr, payoutErr, dailyPayoutErr, ticketSweepErr, analyticsErr, legacyErr error
	var recurrenceErr, pushReceiptErr error
	wg.Add(8)
	go func() {
		defer wg.Done()
		bookingErr = NewBookingWorker(cfg, db, log).Run(ctx)
	}()
	go func() {
		defer wg.Done()
		paymentsErr = reconciler.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		notifyErr = dispatcher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		payoutErr = payoutReconciler.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		dailyPayoutErr = dailyPayouts.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		ticketSweepErr = ticketSweeper.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		analyticsErr = analyticsDispatcher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		recurrenceErr = recurrenceGenerator.Run(ctx)
	}()
	if pushReceipts != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pushReceiptErr = pushReceipts.Run(ctx)
		}()
	}
	if legacySync != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			legacyErr = legacySync.Run(ctx)
		}()
	}
	wg.Wait()

	if bookingErr != nil {
		return fmt.Errorf("booking worker: %w", bookingErr)
	}
	if paymentsErr != nil {
		return fmt.Errorf("payments reconciler: %w", paymentsErr)
	}
	if notifyErr != nil {
		return fmt.Errorf("notification dispatcher: %w", notifyErr)
	}
	if payoutErr != nil {
		return fmt.Errorf("payout reconciler: %w", payoutErr)
	}
	if dailyPayoutErr != nil {
		return fmt.Errorf("daily payout pass: %w", dailyPayoutErr)
	}
	if ticketSweepErr != nil {
		return fmt.Errorf("ticket sweeper: %w", ticketSweepErr)
	}
	if analyticsErr != nil {
		return fmt.Errorf("analytics dispatcher: %w", analyticsErr)
	}
	if recurrenceErr != nil {
		return fmt.Errorf("event recurrence generator: %w", recurrenceErr)
	}
	if pushReceiptErr != nil {
		return fmt.Errorf("push receipt worker: %w", pushReceiptErr)
	}
	if legacyErr != nil {
		return fmt.Errorf("legacy sync: %w", legacyErr)
	}
	return nil
}
