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

// RequestOTP normalizes the phone, enforces rate limits, stores a hashed code,
// and asks the sender to deliver it. Returns the code only when OTPDevExpose.
func (s *Service) RequestOTP(ctx context.Context, rawPhone string) (string, error) {
	p := phone.Normalize(rawPhone)
	if p == "" {
		return "", fmt.Errorf("%w: phone required", domain.ErrValidation)
	}

	perMin, err := s.d.OTP.CountSince(ctx, p, time.Now().Add(-time.Minute))
	if err != nil {
		return "", err
	}
	if perMin >= s.d.Config.OTPPerMin {
		return "", fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation)
	}
	perHour, err := s.d.OTP.CountSince(ctx, p, time.Now().Add(-time.Hour))
	if err != nil {
		return "", err
	}
	if perHour >= s.d.Config.OTPPerHour {
		return "", fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation)
	}

	code, err := otpcode.Generate()
	if err != nil {
		return "", err
	}
	channel, err := s.d.OTPSender.Send(ctx, p, code)
	if err != nil {
		return "", fmt.Errorf("send otp: %w", err)
	}
	now := time.Now()
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   channel,
		ExpiresAt: now.Add(s.d.Config.OTPTTL),
		CreatedAt: now,
	}
	if err := s.d.OTP.Create(ctx, rec); err != nil {
		return "", err
	}
	if s.d.Config.OTPDevExpose {
		return code, nil
	}
	return "", nil
}

// VerifyOTP checks the latest active code for the phone; on success it marks the
// code used, finds-or-creates the user, and returns a token pair.
func (s *Service) VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error) {
	p := phone.Normalize(rawPhone)
	if p == "" || code == "" {
		return nil, fmt.Errorf("%w: phone and code required", domain.ErrValidation)
	}

	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		rec, err := s.d.OTP.LatestActiveByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if rec.Attempts >= maxOTPAttempts {
			return domain.ErrUnauthorized
		}
		if otpcode.Hash(code) != rec.CodeHash {
			_ = s.d.OTP.IncrementAttempts(ctx, rec.ID)
			return domain.ErrUnauthorized
		}
		if err := s.d.OTP.MarkUsed(ctx, rec.ID); err != nil {
			return err
		}

		u, err := s.d.Users.GetByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			now := time.Now()
			u = &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, PreferredLanguage: "ru", PhoneVerifiedAt: &now}
			if err := s.d.Users.Create(ctx, u); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if u.PhoneVerifiedAt == nil {
			now := time.Now()
			u.PhoneVerifiedAt = &now
			if err := s.d.Users.Update(ctx, u); err != nil {
				return err
			}
		}

		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}
