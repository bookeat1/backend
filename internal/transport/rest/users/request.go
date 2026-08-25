package users

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/users"
)

// dateOnlyLayout is the wire format for birth_date: a plain calendar date,
// never a full timestamp (no time-of-day, no timezone).
const dateOnlyLayout = "2006-01-02"

type updateMeRequest struct {
	FullName          *string `json:"full_name" example:"Jane Doe"`
	AvatarURL         *string `json:"avatar_url" example:"https://cdn.example.com/a/jane.png"`
	PreferredLanguage *string `json:"preferred_language" example:"ru"`
	City              *string `json:"city" example:"almaty"`
	// CountryCode is an ISO 3166-1 alpha-2 code, e.g. "KZ".
	CountryCode *string `json:"country_code" example:"KZ"`
	// BirthDate is a plain date, "YYYY-MM-DD".
	BirthDate *string `json:"birth_date" example:"1998-05-04"`
	// CuisineCategoryIDs replaces the foodie profile's picked cuisines when
	// present (including an empty array, which clears all picks). Omit the
	// field entirely to leave existing picks unchanged.
	//
	// The values are ids from the CUISINE dictionary (GET /cuisines) since
	// migration 0079; before that they were ids from restaurant_categories.
	// The wire name is kept for the store builds already installed — renaming
	// it would silently stop their profile writes from taking effect. New
	// clients may send `cuisine_ids` instead; when both are present,
	// `cuisine_ids` wins.
	CuisineCategoryIDs *[]string `json:"cuisine_category_ids"`
	CuisineIDs         *[]string `json:"cuisine_ids"`
}

// phoneChangeRequestRequest asks for an OTP to be sent to a NEW number the
// signed-in user wants to move to.
type phoneChangeRequestRequest struct {
	NewPhone string `json:"new_phone" example:"+77011234567"`
}

// phoneChangeVerifyRequest submits the code delivered to the new number.
type phoneChangeVerifyRequest struct {
	NewPhone string `json:"new_phone" example:"+77011234567"`
	Code     string `json:"code" example:"123456"`
}

func (r updateMeRequest) toInput() (uc.UpdateInput, error) {
	in := uc.UpdateInput{
		FullName:          r.FullName,
		AvatarURL:         r.AvatarURL,
		PreferredLanguage: r.PreferredLanguage,
		City:              r.City,
		CountryCode:       r.CountryCode,
	}
	if r.BirthDate != nil {
		bd, err := time.Parse(dateOnlyLayout, *r.BirthDate)
		if err != nil {
			return uc.UpdateInput{}, fmt.Errorf("%w: birth_date must be YYYY-MM-DD", domain.ErrValidation)
		}
		in.BirthDate = &bd
	}
	if raw, field := r.CuisineCategoryIDs, "cuisine_category_ids"; raw != nil || r.CuisineIDs != nil {
		if r.CuisineIDs != nil {
			raw, field = r.CuisineIDs, "cuisine_ids"
		}
		ids := make([]uuid.UUID, 0, len(*raw))
		for _, s := range *raw {
			id, err := uuid.Parse(s)
			if err != nil {
				return uc.UpdateInput{}, fmt.Errorf("%w: invalid %s entry %q", domain.ErrValidation, field, s)
			}
			ids = append(ids, id)
		}
		in.CuisineIDs = &ids
	}
	return in, nil
}
