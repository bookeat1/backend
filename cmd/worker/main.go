// Command worker runs the background booking janitor: confirm-SLA handling
// (auto-confirm or escalation) and closing finished bookings as completed /
// no_show. It is safe to run several instances — every selection uses
// SELECT ... FOR UPDATE SKIP LOCKED.
package main

import (
	"log/slog"
	"os"

	"backend-core/internal/bootstrap"
	"backend-core/internal/logger"
)

func main() {
	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log := logger.New(cfg.App.LogLevel, cfg.App.LogFormat)
	if err := bootstrap.RunWorker(cfg, log); err != nil {
		log.Error("worker stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
