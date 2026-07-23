// Package logger is a thin wrapper over the standard library log/slog.
// It deliberately avoids any third-party/private logging library so the
// project stays buildable from public modules and the stdlib only.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger writing to stdout at the given level, in the
// given format. level is one of: debug, info, warn, error (case-insensitive,
// default info). format is one of:
//
//   - "json" (default) — one JSON object per line. This is what production
//     needs: Loki/Grafana (and any other line-oriented log shipper) parse it
//     without a regexp.
//   - "text" — slog's human-readable key=value handler, for a developer
//     reading the terminal directly.
//
// An unrecognised format falls back to JSON rather than silently degrading a
// production deployment to unparsable text on a typo.
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
