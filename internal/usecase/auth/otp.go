package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

const maxOTPAttempts = 5

// OTPUseCase is the phone-OTP authentication usecase: request a one-time code
// and verify it (find-or-create the user, then issue a token pair). It is a
// distinct usecase from the core credential Facade (facade.go).
type OTPUseCase interface {
	RequestOTP(ctx context.Context, rawPhone string) (string, error)
	VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error)
}

type otpUseCase struct {
	users   domain.UserRepository
	otp     domain.OTPRepository
	refresh domain.RefreshTokenRepository
	tx      domain.TxManager
	tokens  TokenIssuer
	sender  OTPSender
	cfg     Config
}

// NewOTPUseCase constructs the phone-OTP authentication usecase.
func NewOTPUseCase(
	users domain.UserRepository,
	otp domain.OTPRepository,
	refresh domain.RefreshTokenRepository,
	tx domain.TxManager,
	tokens TokenIssuer,
	sender OTPSender,
	cfg Config,
) OTPUseCase {
	return &otpUseCase{users: users, otp: otp, refresh: refresh, tx: tx, tokens: tokens, sender: sender, cfg: cfg}
}

// RequestOTP normalizes the phone, enforces rate limits, stores a hashed code,
// and asks the sender to deliver it. Returns the code only when OTPDevExpose.
func (o *otpUseCase) RequestOTP(ctx context.Context, rawPhone string) (string, error) {
	p := phone.Normalize(rawPhone)
	if p == "" {
		return "", fmt.Errorf("%w: phone required", domain.ErrValidation)
	}

	perMin, err := o.otp.CountSince(ctx, p, time.Now().Add(-time.Minute))
	if err != nil {
		return "", err
	}
	if perMin >= o.cfg.OTPPerMin {
		return "", fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation)
	}
	perHour, err := o.otp.CountSince(ctx, p, time.Now().Add(-time.Hour))
	if err != nil {
		return "", err
	}
	if perHour >= o.cfg.OTPPerHour {
		return "", fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation)
	}

	code, err := otpcode.Generate()
	if err != nil {
		return "", err
	}
	channel, err := o.sender.Send(ctx, p, code)
	if err != nil {
		return "", fmt.Errorf("send otp: %w", err)
	}
	now := time.Now()
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   channel,
		ExpiresAt: now.Add(o.cfg.OTPTTL),
		CreatedAt: now,
	}
	if err := o.otp.Create(ctx, rec); err != nil {
		return "", err
	}
	if o.cfg.OTPDevExpose {
		return code, nil
	}
	return "", nil
}

// VerifyOTP checks the latest active code for the phone; on success it marks the
// code used, finds-or-creates the user, and returns a token pair.
func (o *otpUseCase) VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error) {
	p := phone.Normalize(rawPhone)
	if p == "" || code == "" {
		return nil, fmt.Errorf("%w: phone and code required", domain.ErrValidation)
	}

	// Read + attempt accounting happen OUTSIDE the transaction: a failed guess
	// must durably increment attempts (if it were inside the tx that returns the
	// auth error, the rollback would discard it and the lockout would never fire).
	rec, err := o.otp.LatestActiveByPhone(ctx, p)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if rec.Attempts >= maxOTPAttempts {
		return nil, domain.ErrUnauthorized
	}
	if otpcode.Hash(code) != rec.CodeHash {
		_ = o.otp.IncrementAttempts(ctx, rec.ID) // committed immediately (no active tx)
		return nil, domain.ErrUnauthorized
	}

	// Correct code: mark used + find-or-create the user + issue tokens atomically.
	var pair *TokenPair
	err = o.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := o.otp.MarkUsed(ctx, rec.ID); err != nil {
			return err
		}

		u, err := o.users.GetByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			now := time.Now()
			u = &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru", PhoneVerifiedAt: &now}
			if err := o.users.Create(ctx, u); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if u.PhoneVerifiedAt == nil {
			now := time.Now()
			u.PhoneVerifiedAt = &now
			if err := o.users.Update(ctx, u); err != nil {
				return err
			}
		}

		pair, err = issuePair(ctx, o.tokens, o.refresh, o.cfg.RefreshTTL, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}
