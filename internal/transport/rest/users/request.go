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
	CuisineCategoryIDs *[]string `json:"cuisine_category_ids"`
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
	if r.CuisineCategoryIDs != nil {
		ids := make([]uuid.UUID, 0, len(*r.CuisineCategoryIDs))
		for _, s := range *r.CuisineCategoryIDs {
			id, err := uuid.Parse(s)
			if err != nil {
				return uc.UpdateInput{}, fmt.Errorf("%w: invalid cuisine_category_ids entry %q", domain.ErrValidation, s)
			}
			ids = append(ids, id)
		}
		in.CuisineCategoryIDs = &ids
	}
	return in, nil
}
