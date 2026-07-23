// Package users is the application logic for reading and updating the current
// user's profile, including their guest-profile fields (country, birth date,
// foodie cuisine preferences) and account deletion.
package users

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/geo"
)

// maxAgeYears bounds BirthDate to a plausible human lifespan: rejects a date
// implying an age over 120.
const maxAgeYears = 120

// Facade exposes the current user's profile read/update/delete operations.
type Facade interface {
	Me(ctx context.Context, id uuid.UUID) (*domain.User, error)
	UpdateMe(ctx context.Context, id uuid.UUID, in UpdateInput) (*domain.User, error)
	// CuisinePreferences returns the category ids of the user's foodie profile.
	CuisinePreferences(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error)
	// DeleteMe soft-deletes and anonymizes the caller's own account, revokes
	// every refresh token and outstanding OTP code tied to it. Idempotent: a
	// repeat call on an already-deleted account is a no-op success.
	DeleteMe(ctx context.Context, id uuid.UUID) error
}

type facade struct {
	users    domain.UserRepository
	cuisines domain.UserCuisinePreferenceRepository
	refresh  domain.RefreshTokenRepository
	otp      domain.OTPRepository
	tx       domain.TxManager
}

// NewFacade constructs the users Facade.
func NewFacade(
	repo domain.UserRepository,
	cuisines domain.UserCuisinePreferenceRepository,
	refresh domain.RefreshTokenRepository,
	otp domain.OTPRepository,
	tx domain.TxManager,
) Facade {
	return &facade{users: repo, cuisines: cuisines, refresh: refresh, otp: otp, tx: tx}
}

// UpdateInput carries the mutable profile fields. A nil pointer leaves the
// existing value unchanged. CuisineCategoryIDs is a *[]uuid.UUID (not a plain
// slice) so a nil pointer ("field omitted") is distinguishable from a
// non-nil-but-empty slice ("clear all my preferences").
type UpdateInput struct {
	FullName           *string
	AvatarURL          *string
	PreferredLanguage  *string
	City               *string
	CountryCode        *string
	BirthDate          *time.Time
	CuisineCategoryIDs *[]uuid.UUID
}

// Me returns the user by id, or ErrNotFound.
func (f *facade) Me(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return f.users.GetByID(ctx, id)
}

// CuisinePreferences returns the user's picked cuisine category ids.
func (f *facade) CuisinePreferences(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	return f.cuisines.ListCategoryIDs(ctx, id)
}

// UpdateMe applies the non-nil fields of in and returns the updated user.
func (f *facade) UpdateMe(ctx context.Context, id uuid.UUID, in UpdateInput) (*domain.User, error) {
	if in.CountryCode != nil && !geo.ValidCountryCode(*in.CountryCode) {
		return nil, fmt.Errorf("%w: country_code must be a valid ISO 3166-1 alpha-2 code", domain.ErrValidation)
	}
	if in.BirthDate != nil {
		if err := validateBirthDate(*in.BirthDate); err != nil {
			return nil, err
		}
	}

	var out *domain.User
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := f.users.GetByID(ctx, id)
		if err != nil {
			return err
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
		if in.CountryCode != nil {
			u.CountryCode = in.CountryCode
		}
		if in.BirthDate != nil {
			bd := *in.BirthDate
			u.BirthDate = &bd
		}
		if err := f.users.Update(ctx, u); err != nil {
			return err
		}
		if in.CuisineCategoryIDs != nil {
			if err := f.cuisines.Replace(ctx, id, *in.CuisineCategoryIDs); err != nil {
				return err
			}
		}
		out = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validateBirthDate rejects a date that is not strictly in the past, or that
// implies an age over maxAgeYears.
func validateBirthDate(bd time.Time) error {
	now := time.Now().UTC()
	if !bd.Before(now) {
		return fmt.Errorf("%w: birth_date must be in the past", domain.ErrValidation)
	}
	oldestAllowed := now.AddDate(-maxAgeYears, 0, 0)
	if bd.Before(oldestAllowed) {
		return fmt.Errorf("%w: birth_date implies an age over %d years", domain.ErrValidation, maxAgeYears)
	}
	return nil
}

// DeleteMe soft-deletes the account, revokes its refresh tokens, and
// invalidates any outstanding OTP code for its (pre-anonymization) phone — all
// inside one transaction so a partial failure never leaves a half-deleted
// account with live sessions.
func (f *facade) DeleteMe(ctx context.Context, id uuid.UUID) error {
	return f.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := f.users.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if u.DeletedAt != nil {
			// Idempotent: sessions/OTP were already invalidated on the first call.
			return nil
		}
		phone := u.Phone

		if err := f.users.Delete(ctx, id); err != nil {
			return err
		}
		if err := f.refresh.RevokeAllByUser(ctx, id); err != nil {
			return err
		}
		if phone != nil {
			if err := f.otp.InvalidateActiveByPhone(ctx, *phone); err != nil {
				return err
			}
		}
		return nil
	})
}
