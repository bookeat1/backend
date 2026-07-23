package middleware

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/logging"
)

// RequestIDHeader is the header a client may set to make BookEat reuse its own
// correlation id (e.g. an upstream gateway that already assigned one), and the
// header the response always carries the resolved id back on.
const RequestIDHeader = "X-Request-Id"

// RequestID assigns every request a correlation id — reusing the caller's
// X-Request-Id when present, generating a UUID otherwise — and makes it
// available two ways:
//
//   - on the response, so a client/gateway can log it next to its own trail;
//   - on the request context, via logging.RequestID and, wrapped into a
//     logger with "request_id" already attached, via logging.FromContext —
//     any usecase or repository that logs through that context is correlated
//     for free, without threading a logger through its constructor.
//
// Must be the first middleware in the chain (or as close to it as possible):
// every later middleware and handler wants the id already on the context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		c.Writer.Header().Set(RequestIDHeader, id)

		ctx := logging.WithRequestID(c.Request.Context(), id)
		log := logging.FromContext(ctx).With(slog.String("request_id", id))
		ctx = logging.WithLogger(ctx, log)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// LogUserContext re-derives the request-scoped logger to also carry user_id
// and role, once the Auth middleware has resolved the caller. It must be
// registered after Auth on any route group that needs it (see
// bootstrap.NewApp) — on a public route there is no AuthUser and this is a
// no-op.
func LogUserContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		if au, ok := GetAuthUser(c.Request.Context()); ok {
			ctx := c.Request.Context()
			log := logging.FromContext(ctx).With(
				slog.String("user_id", au.ID.String()),
				slog.String("role", au.Role),
			)
			c.Request = c.Request.WithContext(logging.WithLogger(ctx, log))
		}
		c.Next()
	}
}
