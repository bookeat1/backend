// Package logger is a thin wrapper over the standard library log/slog.
// It deliberately avoids any third-party/private logging library so the
// project stays buildable from public modules and the stdlib only.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger writing JSON to stdout at the given level.
// level is one of: debug, info, warn, error (case-insensitive).
func New(level string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(level),
	})
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
