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
// worker and the notification dispatcher side by side, and blocks until SIGINT
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var bookingErr, paymentsErr, notifyErr, payoutErr error
	wg.Add(4)
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
	return nil
}
