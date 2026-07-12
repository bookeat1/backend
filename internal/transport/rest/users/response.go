package users

import (
	"time"

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
	EmailVerifiedAt   *time.Time `json:"email_verified_at" example:"2026-07-10T09:00:00Z"`
	PhoneVerifiedAt   *time.Time `json:"phone_verified_at" example:"2026-07-10T09:00:00Z"`
	CreatedAt         time.Time  `json:"created_at" example:"2026-01-15T08:30:00Z"`
}

func fromDomain(u *domain.User) userResponse {
	return userResponse{
		ID: u.ID.String(), Email: u.Email, Phone: u.Phone, FullName: u.FullName,
		Role: string(u.Role), AvatarURL: u.AvatarURL, PreferredLanguage: u.PreferredLanguage,
		City: u.City, EmailVerifiedAt: u.EmailVerifiedAt, PhoneVerifiedAt: u.PhoneVerifiedAt,
		CreatedAt: u.CreatedAt,
	}
}
