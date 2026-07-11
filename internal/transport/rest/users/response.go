package users

import (
	"time"

	"backend-core/internal/domain"
)

type userResponse struct {
	ID                string     `json:"id"`
	Email             *string    `json:"email"`
	Phone             *string    `json:"phone"`
	FullName          string     `json:"full_name"`
	Role              string     `json:"role"`
	AvatarURL         *string    `json:"avatar_url"`
	PreferredLanguage string     `json:"preferred_language"`
	City              *string    `json:"city"`
	EmailVerifiedAt   *time.Time `json:"email_verified_at"`
	PhoneVerifiedAt   *time.Time `json:"phone_verified_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

func fromDomain(u *domain.User) userResponse {
	return userResponse{
		ID: u.ID.String(), Email: u.Email, Phone: u.Phone, FullName: u.FullName,
		Role: string(u.Role), AvatarURL: u.AvatarURL, PreferredLanguage: u.PreferredLanguage,
		City: u.City, EmailVerifiedAt: u.EmailVerifiedAt, PhoneVerifiedAt: u.PhoneVerifiedAt,
		CreatedAt: u.CreatedAt,
	}
}
