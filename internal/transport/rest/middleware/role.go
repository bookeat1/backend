package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
)

// RequireRole aborts the request unless the authenticated user's role is one of
// roles. Must run after Auth, which stores the AuthUser on the context.
func RequireRole(roles ...domain.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		au, ok := GetAuthUser(c.Request.Context())
		if !ok {
			response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
			c.Abort()
			return
		}
		for _, r := range roles {
			if au.Role == string(r) {
				c.Next()
				return
			}
		}
		response.Error(c.Writer, http.StatusForbidden, "forbidden")
		c.Abort()
	}
}
