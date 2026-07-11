package auth

import (
	"time"

	uc "backend-core/internal/usecase/auth"
)

type tokenPairResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func fromPair(p *uc.TokenPair) tokenPairResponse {
	return tokenPairResponse{AccessToken: p.AccessToken, RefreshToken: p.RefreshToken, ExpiresAt: p.ExpiresAt}
}

type otpRequestedResponse struct {
	Sent bool   `json:"sent"`
	Code string `json:"code,omitempty"` // populated only when AUTH_OTP_DEV_EXPOSE=true
}
