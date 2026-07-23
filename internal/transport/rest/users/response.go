package users

import (
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type userResponse struct {
	ID                string     `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	Email             *string    `json:"email" example:"user@example.com"`
	Phone             *string    `json:"phone" example:"+77011234567"`
	FullName          string     `json:"full_name" example:"Jane Doe"`
	Role              string     `json:"role" example:"user"`
	AvatarURL         *string    `json:"avatar_url" example:"https://cdn.example.com/a/jane.png"`
	PreferredLanguage string     `json:"preferred_language" example:"ru"`
	City              *string    `json:"city" example:"almaty"`
	CountryCode       *string    `json:"country_code" example:"KZ"`
	BirthDate         *string    `json:"birth_date" example:"1998-05-04"`
	EmailVerifiedAt   *time.Time `json:"email_verified_at" example:"2026-07-10T09:00:00Z"`
	PhoneVerifiedAt   *time.Time `json:"phone_verified_at" example:"2026-07-10T09:00:00Z"`
	CreatedAt         time.Time  `json:"created_at" example:"2026-01-15T08:30:00Z"`
	// CuisineCategoryIDs is the foodie profile: ids from the restaurants' own
	// category dictionary (restaurant_categories) the user picked.
	CuisineCategoryIDs []string `json:"cuisine_category_ids"`
}

func fromDomain(u *domain.User, cuisineIDs []uuid.UUID) userResponse {
	var birthDate *string
	if u.BirthDate != nil {
		s := u.BirthDate.Format(dateOnlyLayout)
		birthDate = &s
	}
	ids := make([]string, 0, len(cuisineIDs))
	for _, id := range cuisineIDs {
		ids = append(ids, id.String())
	}
	return userResponse{
		ID: u.ID.String(), Email: u.Email, Phone: u.Phone, FullName: u.FullName,
		Role: string(u.Role), AvatarURL: u.AvatarURL, PreferredLanguage: u.PreferredLanguage,
		City: u.City, CountryCode: u.CountryCode, BirthDate: birthDate,
		EmailVerifiedAt: u.EmailVerifiedAt, PhoneVerifiedAt: u.PhoneVerifiedAt,
		CreatedAt: u.CreatedAt, CuisineCategoryIDs: ids,
	}
}
