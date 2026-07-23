package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// OTPCode is a single issued phone one-time code. CodeHash is sha256(code).
type OTPCode struct {
	ID        uuid.UUID
	Phone     string
	CodeHash  string
	Channel   string
	Attempts  int
	UsedAt    *time.Time
	ExpiresAt time.Time
	CreatedAt time.Time
}

// OTPRepository persists OTP codes.
type OTPRepository interface {
	Create(ctx context.Context, c *OTPCode) error
	// LatestActiveByPhone returns the newest unused, unexpired code for phone,
	// or ErrNotFound.
	LatestActiveByPhone(ctx context.Context, phone string) (*OTPCode, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
	IncrementAttempts(ctx context.Context, id uuid.UUID) error
	// CountSince counts codes created for phone at or after ts (for rate limits).
	CountSince(ctx context.Context, phone string, ts time.Time) (int, error)
	// InvalidateActiveByPhone marks every still-active (unused, unexpired) code
	// for phone as used, so none of them can complete a login. Used on account
	// deletion, before the phone number is anonymized off the user row and
	// potentially reused by a new signup. A no-op success when none are active.
	InvalidateActiveByPhone(ctx context.Context, phone string) error
}
