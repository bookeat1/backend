// Package middleware holds shared Gin middleware.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/transport/rest/response"
	"backend-core/internal/usecase/auth"
)

type ctxKey struct{}

// AuthUser is the authenticated principal stored on the request context.
type AuthUser struct {
	ID   uuid.UUID
	Role string
}

// Auth verifies the Bearer access token and stores an AuthUser on the context.
// Rejects missing/invalid tokens with 401.
func Auth(issuer auth.TokenIssuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			response.Error(c.Writer, http.StatusUnauthorized, "missing bearer token")
			c.Abort()
			return
		}
		id, role, err := issuer.ParseAccess(strings.TrimSpace(h[7:]))
		if err != nil {
			response.Error(c.Writer, http.StatusUnauthorized, "invalid token")
			c.Abort()
			return
		}
		c.Set(ctxKeyString, AuthUser{ID: id, Role: role})
		c.Next()
	}
}

const ctxKeyString = "auth_user"

// GetAuthUser returns the AuthUser set by Auth.
func GetAuthUser(c *gin.Context) (AuthUser, bool) {
	v, ok := c.Get(ctxKeyString)
	if !ok {
		return AuthUser{}, false
	}
	au, ok := v.(AuthUser)
	return au, ok
}
