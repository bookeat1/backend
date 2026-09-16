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
	// CuisinePreferences returns the cuisine-dictionary ids of the user's
	// foodie profile (domain.Cuisine, migration 0079).
	CuisinePreferences(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error)
	// DeleteMe soft-deletes and anonymizes the caller's own account, revokes
	// every refresh token and outstanding OTP code tied to it. Idempotent: a
	// repeat call on an already-deleted account is a no-op success.
	DeleteMe(ctx context.Context, id uuid.UUID) error
	// SetAvatarURL points the user's avatar at an already-stored image.
	//
	// A named method rather than "just call UpdateMe with one field": the
	// avatar upload writes on behalf of the CALLER only, and a narrow method is
	// what the transport layer can be given without also handing it the ability
	// to rewrite a name, a city or a birth date.
	SetAvatarURL(ctx context.Context, id uuid.UUID, url string) error
	// GetFoodieProfile returns the caller's "Фуди-профиль" wizard state
	// (mobile PR #222): cuisines/diets/allergies picks plus the optional
	// budget tier. A user who never opened the wizard gets empty slices and
	// a nil Budget, never ErrNotFound.
	GetFoodieProfile(ctx context.Context, id uuid.UUID) (domain.FoodieProfile, error)
	// ReplaceFoodieProfile overwrites the caller's ENTIRE foodie profile in
	// one call — replace semantics, matching the wizard saving its whole
	// draft on the last screen rather than one field at a time.
	ReplaceFoodieProfile(ctx context.Context, id uuid.UUID, in ReplaceFoodieProfileInput) (domain.FoodieProfile, error)
}

type facade struct {
	users    domain.UserRepository
	cuisines domain.UserCuisinePreferenceRepository
	foodie   domain.FoodieProfileRepository
	refresh  domain.RefreshTokenRepository
	otp      domain.OTPRepository
	tx       domain.TxManager
}

// NewFacade constructs the users Facade.
func NewFacade(
	repo domain.UserRepository,
	cuisines domain.UserCuisinePreferenceRepository,
	foodie domain.FoodieProfileRepository,
	refresh domain.RefreshTokenRepository,
	otp domain.OTPRepository,
	tx domain.TxManager,
) Facade {
	return &facade{users: repo, cuisines: cuisines, foodie: foodie, refresh: refresh, otp: otp, tx: tx}
}

// UpdateInput carries the mutable profile fields. A nil pointer leaves the
// existing value unchanged. CuisineIDs is a *[]uuid.UUID (not a plain
// slice) so a nil pointer ("field omitted") is distinguishable from a
// non-nil-but-empty slice ("clear all my preferences").
//
// Since migration 0079 these are ids from the CUISINE dictionary; they used to
// be ids from restaurant_categories (venue types), which is why the wire field
// is still called cuisine_category_ids — see transport/rest/users.
type UpdateInput struct {
	FullName          *string
	AvatarURL         *string
	PreferredLanguage *string
	City              *string
	CountryCode       *string
	BirthDate         *time.Time
	CuisineIDs        *[]uuid.UUID
}

// Me returns the user by id, or ErrNotFound.
func (f *facade) Me(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return f.users.GetByID(ctx, id)
}

// CuisinePreferences returns the user's picked cuisine ids.
func (f *facade) CuisinePreferences(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	return f.cuisines.ListCuisineIDs(ctx, id)
}

// SetAvatarURL stores the avatar URL on the user's profile.
func (f *facade) SetAvatarURL(ctx context.Context, id uuid.UUID, url string) error {
	_, err := f.UpdateMe(ctx, id, UpdateInput{AvatarURL: &url})
	return err
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
		if in.CuisineIDs != nil {
			if err := f.cuisines.Replace(ctx, id, *in.CuisineIDs); err != nil {
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

// ReplaceFoodieProfileInput is the whole "Фуди-профиль" wizard draft, saved
// in one PUT (replace semantics: every field is overwritten, never merged).
// Budget is a *string (not a plain string) so "field omitted" would be
// distinguishable from "explicitly cleared" if the wire layer ever needs
// that — today the wizard has no partial-save mode, so the transport layer
// always passes it, nil meaning "no budget picked".
type ReplaceFoodieProfileInput struct {
	Cuisines  []string
	Diets     []string
	Allergies []string
	Budget    *string
}

// GetFoodieProfile returns the caller's foodie profile. Empty slices and a
// nil Budget for a user who never opened the wizard — never ErrNotFound,
// same convention as CuisinePreferences.
func (f *facade) GetFoodieProfile(ctx context.Context, id uuid.UUID) (domain.FoodieProfile, error) {
	u, err := f.users.GetByID(ctx, id)
	if err != nil {
		return domain.FoodieProfile{}, err
	}
	prefs, err := f.foodie.Get(ctx, id)
	if err != nil {
		return domain.FoodieProfile{}, err
	}
	return domain.FoodieProfile{
		Cuisines: prefs.Cuisines, Diets: prefs.Diets, Allergies: prefs.Allergies,
		Budget: u.FoodieBudgetTier,
	}, nil
}

// ReplaceFoodieProfile validates in against the wizard's own rules (option
// ids exist, the 5-cuisine cap, no_diet's exclusivity, a known budget tier)
// and then overwrites the caller's whole foodie profile atomically: the
// users.foodie_budget_tier column and all three preference tables are
// written inside ONE transaction, so a rejected write never leaves budget
// updated but preferences stale (or vice versa).
func (f *facade) ReplaceFoodieProfile(ctx context.Context, id uuid.UUID, in ReplaceFoodieProfileInput) (domain.FoodieProfile, error) {
	if err := validateFoodieCuisines(in.Cuisines); err != nil {
		return domain.FoodieProfile{}, err
	}
	if err := validateFoodieDiets(in.Diets); err != nil {
		return domain.FoodieProfile{}, err
	}
	if err := validateFoodieAllergies(in.Allergies); err != nil {
		return domain.FoodieProfile{}, err
	}
	if err := validateFoodieBudget(in.Budget); err != nil {
		return domain.FoodieProfile{}, err
	}

	var out domain.FoodieProfile
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := f.users.GetByID(ctx, id)
		if err != nil {
			return err
		}
		u.FoodieBudgetTier = in.Budget
		if err := f.users.Update(ctx, u); err != nil {
			return err
		}
		prefs := domain.FoodieProfilePreferences{Cuisines: in.Cuisines, Diets: in.Diets, Allergies: in.Allergies}
		if err := f.foodie.Replace(ctx, id, prefs); err != nil {
			return err
		}
		out, err = f.GetFoodieProfile(ctx, id)
		return err
	})
	if err != nil {
		return domain.FoodieProfile{}, err
	}
	return out, nil
}

// validateFoodieCuisines rejects an unknown cuisine id or more than
// domain.FoodieCuisineSelectionLimit entries. The 5-tile cap is a hard block
// on the client too (foodie-profile-selection.ts) — this is the
// server-side backstop against a client bug or a hand-rolled request.
func validateFoodieCuisines(ids []string) error {
	if len(ids) > domain.FoodieCuisineSelectionLimit {
		return fmt.Errorf("%w: cuisines: at most %d allowed, got %d",
			domain.ErrValidation, domain.FoodieCuisineSelectionLimit, len(ids))
	}
	for _, id := range ids {
		if !domain.ValidFoodieCuisineID(id) {
			return fmt.Errorf("%w: cuisines: unknown id %q", domain.ErrValidation, id)
		}
	}
	return nil
}

// validateFoodieDiets rejects an unknown diet id, and rejects
// domain.FoodieDietExclusiveID ("no_diet") appearing alongside any other
// diet — the two are mutually exclusive by definition.
func validateFoodieDiets(ids []string) error {
	hasExclusive := false
	for _, id := range ids {
		if !domain.ValidFoodieDietID(id) {
			return fmt.Errorf("%w: diets: unknown id %q", domain.ErrValidation, id)
		}
		if id == domain.FoodieDietExclusiveID {
			hasExclusive = true
		}
	}
	if hasExclusive && len(ids) > 1 {
		return fmt.Errorf("%w: diets: %q cannot be combined with any other diet",
			domain.ErrValidation, domain.FoodieDietExclusiveID)
	}
	return nil
}

// validateFoodieAllergies rejects an unknown allergy id. No limit, no
// exclusivity.
func validateFoodieAllergies(ids []string) error {
	for _, id := range ids {
		if !domain.ValidFoodieAllergyID(id) {
			return fmt.Errorf("%w: allergies: unknown id %q", domain.ErrValidation, id)
		}
	}
	return nil
}

// validateFoodieBudget rejects a non-nil tier that is not one of
// domain.FoodieBudgetTierIDs. nil (the step was skipped) is always valid.
func validateFoodieBudget(tier *string) error {
	if tier == nil {
		return nil
	}
	if !domain.ValidFoodieBudgetTier(*tier) {
		return fmt.Errorf("%w: budget: unknown tier %q", domain.ErrValidation, *tier)
	}
	return nil
}
