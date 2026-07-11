// Package users is the application logic for reading and updating the current
// user's profile.
package users

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes profile read/update operations.
type Facade struct{ users domain.UserRepository }

func NewFacade(repo domain.UserRepository) *Facade { return &Facade{users: repo} }

// UpdateInput carries the mutable profile fields. A nil pointer leaves the
// existing value unchanged.
type UpdateInput struct {
	FullName          *string
	AvatarURL         *string
	PreferredLanguage *string
	City              *string
}

// Me returns the user by id, or ErrNotFound.
func (f *Facade) Me(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return f.users.GetByID(ctx, id)
}

// UpdateMe applies the non-nil fields of in and returns the updated user.
func (f *Facade) UpdateMe(ctx context.Context, id uuid.UUID, in UpdateInput) (*domain.User, error) {
	u, err := f.users.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.FullName != nil {
		u.FullName = *in.FullName
	}
	if in.AvatarURL != nil {
		u.AvatarURL = in.AvatarURL
	}
	if in.PreferredLanguage != nil {
		u.PreferredLanguage = *in.PreferredLanguage
	}
	if in.City != nil {
		u.City = in.City
	}
	if err := f.users.Update(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}
