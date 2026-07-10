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
}
