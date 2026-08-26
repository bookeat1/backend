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

// otpVerifyResponse is the token pair PLUS the one fact only the OTP login
// knows: whether this very call created the account. It is a separate DTO
// rather than a field on tokenPairResponse because the flag would be a lie on
// every other endpoint that returns a pair — refresh and email/password login
// never create anybody (a hardcoded false invites the app to treat a
// re-login as "returning" evidence), and signup always does. Purely additive:
// older apps ignore the extra key.
type otpVerifyResponse struct {
	tokenPairResponse
	IsNewUser bool `json:"is_new_user" example:"true"`
}

func fromOTPPair(p *uc.TokenPair) otpVerifyResponse {
	return otpVerifyResponse{tokenPairResponse: fromPair(p), IsNewUser: p.IsNewUser}
}

type otpRequestedResponse struct {
	Sent bool   `json:"sent" example:"true"`
	Code string `json:"code,omitempty" example:"123456"` // populated only when AUTH_OTP_DEV_EXPOSE=true
}
