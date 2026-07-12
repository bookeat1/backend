// Package middleware holds shared Gin middleware.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	"backend-core/internal/usecase/auth"
)

// AuthUser is the authenticated principal stored on the request context.
type AuthUser struct {
	ID   uuid.UUID
	Role string
}

// Auth verifies the Bearer access token, loads the user from the DB, and
// stores an AuthUser on the context. Rejects missing/invalid tokens and
// deleted/inactive users with 401.
func Auth(issuer auth.TokenIssuer, users domain.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			response.Error(c.Writer, http.StatusUnauthorized, "missing bearer token")
			c.Abort()
			return
		}
		id, _, err := issuer.ParseAccess(strings.TrimSpace(h[7:]))
		if err != nil {
			response.Error(c.Writer, http.StatusUnauthorized, "invalid token")
			c.Abort()
			return
		}
		u, err := users.GetByID(c.Request.Context(), id)
		if err != nil { // ErrNotFound (deleted) or any load error → not authorized
			response.Error(c.Writer, http.StatusUnauthorized, "invalid token")
			c.Abort()
			return
		}
		if !u.IsActive {
			response.Error(c.Writer, http.StatusUnauthorized, "account is inactive")
			c.Abort()
			return
		}
		ctx := context.WithValue(c.Request.Context(), authUserKey{}, AuthUser{ID: u.ID, Role: string(u.Role)})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// authUserKey is the private context key under which Auth stores the AuthUser —
// a typed key (not a string) so lookups are type-safe and decoupled from gin.
type authUserKey struct{}

// GetAuthUser returns the AuthUser stored by Auth on the request context.
func GetAuthUser(ctx context.Context) (AuthUser, bool) {
	au, ok := ctx.Value(authUserKey{}).(AuthUser)
	return au, ok
}
