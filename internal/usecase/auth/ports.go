// Package auth is the authentication application logic: password + phone-OTP
// login, JWT issuance, and refresh-token rotation.
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TokenIssuer issues and verifies access tokens. Implemented by
// infrastructure/token.RSAIssuer.
type TokenIssuer interface {
	IssueAccess(userID uuid.UUID, role string) (string, time.Time, error)
	ParseAccess(token string) (uuid.UUID, string, error)
}

// OTPSender delivers an OTP code and returns the channel used (one of the
// domain.OTPChannel* constants). Implemented by infrastructure/otpsender:
// Waterfall when any provider is configured, Stub when none is.
//
// The hint is advisory ordering built by the usecase from what it knows about
// the phone (see RequestOTP). A sender is free to ignore it, and MUST NOT fail
// because of it: ordering is an optimization, delivery is the contract.
type OTPSender interface {
	Send(ctx context.Context, phone, code string, hint domain.OTPSendHint) (string, error)
}

// TokenPair is the credential set returned to a client on successful auth.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Config holds auth timing and OTP policy.
type Config struct {
	RefreshTTL   time.Duration
	OTPTTL       time.Duration
	OTPPerMin    int
	OTPPerHour   int
	OTPDevExpose bool
}
