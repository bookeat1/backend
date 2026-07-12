package domain

import (
	"context"

	"github.com/google/uuid"
)

// UserCredential holds a user's bcrypt password hash. Absent for OTP-only users.
type UserCredential struct {
	UserID       uuid.UUID
	PasswordHash string
}

// UserCredentialRepository persists password hashes. GetByUserID returns
// ErrNotFound when the user has no password credential.
type UserCredentialRepository interface {
	Upsert(ctx context.Context, c *UserCredential) error
	GetByUserID(ctx context.Context, userID uuid.UUID) (*UserCredential, error)
}
