package users

import uc "backend-core/internal/usecase/users"

type updateMeRequest struct {
	FullName          *string `json:"full_name"`
	AvatarURL         *string `json:"avatar_url"`
	PreferredLanguage *string `json:"preferred_language"`
	City              *string `json:"city"`
}

func (r updateMeRequest) toInput() uc.UpdateInput {
	return uc.UpdateInput{
		FullName:          r.FullName,
		AvatarURL:         r.AvatarURL,
		PreferredLanguage: r.PreferredLanguage,
		City:              r.City,
	}
}
