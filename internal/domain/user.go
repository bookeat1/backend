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
// least one is always present (unless the account was deleted — see
// DeletedAt). ID equals the original Supabase auth.users id.
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
	// CountryCode is an ISO 3166-1 alpha-2 country code (e.g. "KZ"), used to
	// tell tourists from locals. Nullable until the guest fills their profile.
	CountryCode *string
	// BirthDate is a plain calendar date (time-of-day is always midnight UTC).
	// Nullable until the guest fills their profile.
	BirthDate       *time.Time
	EmailVerifiedAt *time.Time
	PhoneVerifiedAt *time.Time
	// DeletedAt marks a soft-deleted account: non-nil means the account was
	// closed by its owner. The row is kept (bookings/payments reference it) but
	// personal data has been scrubbed — see UserRepository.Delete.
	DeletedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserRepository persists users. Get* return ErrNotFound when absent.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByPhone(ctx context.Context, phone string) (*User, error)
	Update(ctx context.Context, u *User) error
	// Delete soft-deletes and anonymizes the user in one atomic write: sets
	// DeletedAt, clears email/phone/full_name/avatar/birth_date/country_code,
	// and flips IsActive false. Bookings/payments keep their user_id reference
	// unchanged — only the users row itself is scrubbed. Idempotent: calling it
	// again on an already-deleted user is a no-op success, not an error.
	// Returns ErrNotFound only when no user with that id exists at all.
	Delete(ctx context.Context, id uuid.UUID) error
}

// UserCuisinePreferenceRepository stores a user's foodie-profile cuisine
// preferences: a many-to-many link to the existing restaurant_categories
// dictionary (the same reference list restaurants themselves are tagged
// with — no separate cuisine dictionary is invented).
type UserCuisinePreferenceRepository interface {
	// ListCategoryIDs returns the category ids the user picked, in no
	// particular order. Empty slice, never an error, when the user picked none.
	ListCategoryIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
	// Replace deletes the user's existing preferences and inserts categoryIDs.
	// An unknown category id fails the whole call with ErrValidation (FK
	// violation). Replace itself is NOT atomic against a partial failure — the
	// delete and each insert are separate statements — so callers MUST run it
	// inside a domain.TxManager.WithinTx (as usecase/users.UpdateMe does) so a
	// rejected id rolls the delete back too, leaving the previous preferences
	// intact rather than silently clearing them.
	Replace(ctx context.Context, userID uuid.UUID, categoryIDs []uuid.UUID) error
}
