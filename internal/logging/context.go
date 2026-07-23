// Package logging carries request-scoped correlation (request id, logger with
// attached fields) through context.Context so every layer — transport,
// usecase, repository — can log a line that is trivially joinable to the rest
// of the request's trail, without threading a logger through every function
// signature.
//
// It deliberately has no dependency on gin or any transport framework: the
// HTTP-specific wiring (reading X-Request-Id, writing the response header)
// lives in internal/transport/rest/middleware, which imports this package.
package logging

import (
	"context"
	"log/slog"
)

// ctxKey is a private, typed context key so lookups here can never collide
// with a key set by unrelated code (same pattern as
// transport/rest/middleware.authUserKey).
type ctxKey int

const (
	requestIDKey ctxKey = iota
	loggerKey
)

// WithRequestID returns a copy of ctx carrying the given request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id stored on ctx, and whether one was found.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// WithLogger returns a copy of ctx carrying log as the request-scoped logger
// that FromContext will return. Callers typically pass a logger already
// enriched with slog.With(...) — e.g. the request id, and, once known, the
// authenticated user id and role.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, log)
}

// FromContext returns the logger attached to ctx by WithLogger. When none was
// attached (a context that never went through the request-id middleware —
// unit tests, one-off scripts), it falls back to slog.Default() so callers
// never need a nil check.
func FromContext(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}

// With returns the context's logger with extra structured fields attached. It
// is a convenience for the common one-off case
// (logging.With(ctx, "booking_id", id).Info(...)) and does not mutate ctx —
// callers that log more than once with the same extra fields should call
// FromContext(ctx).With(args...) themselves and reuse the result.
func With(ctx context.Context, args ...any) *slog.Logger {
	return FromContext(ctx).With(args...)
}
