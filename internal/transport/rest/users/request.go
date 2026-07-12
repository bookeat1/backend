package users

import uc "backend-core/internal/usecase/users"

type updateMeRequest struct {
	FullName          *string `json:"full_name" example:"Jane Doe"`
	AvatarURL         *string `json:"avatar_url" example:"https://cdn.example.com/a/jane.png"`
	PreferredLanguage *string `json:"preferred_language" example:"ru"`
	City              *string `json:"city" example:"almaty"`
}

func (r updateMeRequest) toInput() uc.UpdateInput {
	return uc.UpdateInput{
		FullName:          r.FullName,
		AvatarURL:         r.AvatarURL,
		PreferredLanguage: r.PreferredLanguage,
		City:              r.City,
	}
}
