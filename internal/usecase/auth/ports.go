// Package auth is the authentication application logic: password + phone-OTP
// login, JWT issuance, and refresh-token rotation.
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TokenIssuer issues and verifies access tokens. Implemented by
// infrastructure/token.RSAIssuer.
type TokenIssuer interface {
	IssueAccess(userID uuid.UUID, role string) (string, time.Time, error)
	ParseAccess(token string) (uuid.UUID, string, error)
}

// OTPSender delivers an OTP code and returns the channel used. Implemented by
// infrastructure/otpsender.Stub.
type OTPSender interface {
	Send(ctx context.Context, phone, code string) (string, error)
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
