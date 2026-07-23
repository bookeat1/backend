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
	if err := bootstrap.Run(cfg, log); err != nil {
		log.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
