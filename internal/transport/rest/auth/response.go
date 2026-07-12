package auth

import (
	"time"

	uc "backend-core/internal/usecase/auth"
)

type tokenPairResponse struct {
	AccessToken  string    `json:"access_token" example:"eyJhbGciOiJSUzI1NiIsImtpZCI6..."`
	RefreshToken string    `json:"refresh_token" example:"9c8b7a6f-4d3e-2b1a-...-refresh"`
	ExpiresAt    time.Time `json:"expires_at" example:"2026-07-12T18:30:00Z"`
}

func fromPair(p *uc.TokenPair) tokenPairResponse {
	return tokenPairResponse{AccessToken: p.AccessToken, RefreshToken: p.RefreshToken, ExpiresAt: p.ExpiresAt}
}

type otpRequestedResponse struct {
	Sent bool   `json:"sent" example:"true"`
	Code string `json:"code,omitempty" example:"123456"` // populated only when AUTH_OTP_DEV_EXPOSE=true
}
