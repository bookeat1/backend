package main

import (
	"log/slog"
	"net/http"
	"os"

	"backend-core/internal/bootstrap"
	"backend-core/internal/logger"
	"backend-core/internal/transport/rest/response"
)

// Placeholder entry point. Real wiring belongs in internal/bootstrap (NewDeps,
// app.Run). This stub loads config and serves an unauthenticated /health check
// so the scaffold runs end-to-end; replace it with the Gin app once bootstrap
// grows a Deps/App.
func main() {
	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	log := logger.New(cfg.App.LogLevel)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		response.OK(w, map[string]string{"status": "ok"})
	})

	log.Info("http server starting", slog.String("addr", cfg.App.URL))
	if err := http.ListenAndServe(cfg.App.URL, mux); err != nil {
		log.Error("http server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
