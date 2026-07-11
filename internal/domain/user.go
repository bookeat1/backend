package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Role is a user's authorization level, stored as VARCHAR.
type Role string

const (
	RoleUser       Role = "user"
	RoleRestaurant Role = "restaurant"
	RoleAdmin      Role = "admin"
)

// User is a person who can authenticate. Email and Phone are optional but at
// least one is always present. ID equals the original Supabase auth.users id.
type User struct {
	ID                uuid.UUID
	Email             *string
	Phone             *string
	FullName          string
	Role              Role
	IsActive          bool
	AvatarURL         *string
	PreferredLanguage string
	City              *string
	EmailVerifiedAt   *time.Time
	PhoneVerifiedAt   *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// UserRepository persists users. Get* return ErrNotFound when absent.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByPhone(ctx context.Context, phone string) (*User, error)
	Update(ctx context.Context, u *User) error
}
