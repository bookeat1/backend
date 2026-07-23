package bootstrap

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
)

// RunWorker starts the background booking worker and blocks until SIGINT or
// SIGTERM. The current pass is allowed to finish: the signal cancels the
// context, the worker returns from Run between ticks, and the pool is closed
// afterwards.
func RunWorker(cfg Config, log *slog.Logger) error {
	db, err := NewDB(cfg.DB.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return NewBookingWorker(cfg, db, log).Run(ctx)
}
