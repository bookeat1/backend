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

// guestBookingLinker hands a guest the bookings that were made for their phone
// number before they had an account. Implemented by
// infrastructure/postgres/booking.Repository.AttachOrphanedByPhone.
//
// A narrow port rather than the whole domain.BookingRepository on purpose: this
// package must be able to give a booking an owner and nothing else — it can
// neither read, cancel nor rewrite one. It is called INSIDE the usecase's
// transaction, so the implementation must honour the ambient tx.
type guestBookingLinker interface {
	// AttachOrphanedByPhone assigns userID to every booking whose
	// phone_normalized equals phoneNormalized AND whose user_id is NULL,
	// returning the number of rows attached. It must never touch a booking that
	// already has an owner, and calling it twice must attach zero the second
	// time.
	AttachOrphanedByPhone(ctx context.Context, userID uuid.UUID, phoneNormalized string) (int64, error)
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

	// TestAccountPhone / TestAccountCode enable the App Store review account:
	// one number that logs in with a fixed code and no message sent. Both empty
	// = the feature does not exist and every number takes the ordinary path.
	// Any human phone format is accepted, it is normalized once at construction.
	// See test_account.go for the full rationale and rules.
	TestAccountPhone string
	TestAccountCode  string
}
