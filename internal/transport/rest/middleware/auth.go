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

// OptionalAuth is Auth for a route a caller may legitimately reach with no
// account at all — a guest checkout link opened without ever logging in
// (spec: "a guest may be without an account"). It attaches an AuthUser on the
// context when the request carries a valid, active bearer token, exactly like
// Auth; a missing header, a malformed/expired token, an unknown user or an
// inactive account is NOT an error here — the request simply proceeds with no
// AuthUser, and GetAuthUser reports ok=false. The handler downstream then
// builds its own "anonymous guest" actor.
//
// It never widens access on its own: whatever the caller is allowed to do
// anonymously vs. as their own account is still decided entirely inside the
// usecase (e.g. usecase/payments.authorizeRead/authorizeCreate), the same as
// the accounts that DO authenticate on a route gated by Auth. This exists
// because bookings has no equivalent unauthenticated entry point to copy —
// every booking route requires Auth — so a guest payment link is a genuinely
// new case, not a reinvention of an existing mechanism.
func OptionalAuth(issuer auth.TokenIssuer, users domain.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			c.Next()
			return
		}
		id, _, err := issuer.ParseAccess(strings.TrimSpace(h[7:]))
		if err != nil {
			c.Next()
			return
		}
		u, err := users.GetByID(c.Request.Context(), id)
		if err != nil || !u.IsActive {
			c.Next()
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
