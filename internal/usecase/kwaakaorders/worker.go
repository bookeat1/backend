package kwaakaorders

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Tick runs one pass of every stage. Stages are independent: one failing does
// not stop the next.
//
// Order matters: cancel runs BEFORE send. A booking cancelled while its row waits
// for a retry (after 429/401) must be flagged by CancelSweep before SendPass
// leases the row, otherwise the retry POSTs a create for a cancelled booking.
func (w *Worker) Tick(ctx context.Context) {
	stages := []struct {
		name string
		fn   func(context.Context) error
	}{{"claim", w.ClaimPass}, {"cancel", w.CancelSweep}, {"send", w.SendPass}, {"reschedule", w.RescheduleSweep},
		{"inbox", w.ProcessInbox}, {"poll", w.PollPass}, {"prune", w.PruneInbox}}
	for _, st := range stages {
		if err := st.fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("kwaaka orders stage failed", slog.String("stage", st.name), slog.String("error", err.Error()))
		}
	}
}

// Run ticks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	// Webhooks are applied on a faster beat than the send loop.
	inbox := time.NewTicker(5 * time.Second)
	defer inbox.Stop()
	w.log.Info("kwaaka orders worker started", slog.Duration("tick", tick), slog.Bool("sending_enabled", w.cfg.Enabled))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-inbox.C:
			if err := w.ProcessInbox(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Error("kwaaka orders stage failed", slog.String("stage", "inbox"), slog.String("error", err.Error()))
			}
		case <-t.C:
			w.Tick(ctx)
		}
	}
}
