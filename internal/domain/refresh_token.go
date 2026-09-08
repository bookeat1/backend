package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RefreshToken is a hashed, rotating refresh credential.
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	UserAgent *string
	CreatedAt time.Time
}

// RefreshTokenRepository persists refresh tokens. GetByHash returns ErrNotFound
// when no row matches.
type RefreshTokenRepository interface {
	Create(ctx context.Context, t *RefreshToken) error
	GetByHash(ctx context.Context, tokenHash string) (*RefreshToken, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	// RevokeAllByUser revokes every not-yet-revoked refresh token for userID.
	// Used on account deletion so no outstanding refresh token can mint a new
	// access token for the user afterwards. A no-op success when the user has
	// no active tokens.
	RevokeAllByUser(ctx context.Context, userID uuid.UUID) error
}
