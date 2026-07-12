package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/pressly/goose/v3"

	"backend-core/internal/bootstrap"
	"backend-core/migrations"
)

// Usage: go run ./cmd/migrate/migrate.go [up|down|status]
func main() {
	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	db, err := bootstrap.NewSQLDB(cfg.DB.Postgres)
	if err != nil {
		slog.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		slog.Error("set dialect", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if err := goose.RunContext(context.Background(), cmd, db, "."); err != nil {
		slog.Error("migrate", slog.String("cmd", cmd), slog.String("error", err.Error()))
		os.Exit(1)
	}
}
