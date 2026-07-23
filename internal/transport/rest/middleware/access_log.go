package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/logging"
)

// AccessLog writes one structured line per request: method, route pattern
// (not the path with ids substituted in, so requests to the same endpoint
// group together), status, duration, response size, client ip, user agent —
// plus request_id (and user_id/role, once authenticated) via whatever logger
// RequestID/LogUserContext already attached to the request context.
//
// Level follows the response status: 5xx is an operational failure (error),
// 4xx is a rejected request (warn, not an error — a bad request is normal
// traffic), everything else is info.
//
// Register AFTER RequestID and Recovery so that: (a) the logger already has
// request_id attached, and (b) a panic recovered downstream is still measured
// and logged here as its converted 500, not lost. The logger it writes with is
// whatever RequestID (and, once authenticated, LogUserContext) attached to the
// request context — falling back to slog.Default() if, unusually, neither ran
// (see logging.FromContext).
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		c.Next()

		// FullPath() is the registered route template (e.g.
		// "/api/v1/bookings/:id"); it is empty when no route matched (404), in
		// which case the raw path is the best we have.
		route := c.FullPath()
		if route == "" {
			route = path
		}
		status := c.Writer.Status()
		fields := []any{
			slog.String("method", method),
			slog.String("path", route),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("size", c.Writer.Size()),
			slog.String("ip", c.ClientIP()),
			slog.String("user_agent", c.Request.UserAgent()),
		}

		log := logging.FromContext(c.Request.Context())
		switch {
		case status >= 500:
			log.Error("http.request", fields...)
		case status >= 400:
			log.Warn("http.request", fields...)
		default:
			log.Info("http.request", fields...)
		}
	}
}
