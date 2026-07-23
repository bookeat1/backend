package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"backend-core/internal/logging"
	"backend-core/internal/transport/rest/response"
)

// Recovery catches a panic anywhere downstream, logs it with its stack trace
// through the request-scoped logger (so it carries request_id like every
// other line), and answers with the standard 500 envelope instead of letting
// the panic reach net/http (which would dump to stderr and close the
// connection without the JSON envelope every other error uses).
//
// Replaces gin.Recovery() in the middleware chain — same outcome class (a
// panic never crashes the process, the client gets a 500), but wired into
// this package's logging and response envelope.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logging.FromContext(c.Request.Context()).Error("http.panic",
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)
			if !c.Writer.Written() {
				response.Error(c.Writer, http.StatusInternalServerError, "internal server error")
			}
			c.Abort()
		}()
		c.Next()
	}
}
