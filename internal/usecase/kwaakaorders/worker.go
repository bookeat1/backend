package kwaakaorders

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Tick runs one pass of every stage. Stages are independent: one failing does
// not stop the next.
func (w *Worker) Tick(ctx context.Context) {
	stages := []struct {
		name string
		fn   func(context.Context) error
	}{{"claim", w.ClaimPass}, {"send", w.SendPass}, {"cancel", w.CancelSweep}, {"reschedule", w.RescheduleSweep}}
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
	w.log.Info("kwaaka orders worker started", slog.Duration("tick", tick), slog.Bool("sending_enabled", w.cfg.Enabled))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.Tick(ctx)
		}
	}
}
