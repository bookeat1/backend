package auth

import (
	"context"
	"testing"
	"time"

	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
)

func newTestService(t *testing.T) (*Service, *stubSender) {
	t.Helper()
	iss, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	sender := &stubSender{}
	return NewService(Deps{
		Users:       newFakeUsers(),
		Credentials: newFakeCreds(),
		Refresh:     newFakeRefresh(),
		OTP:         newFakeOTP(),
		Tx:          noTx{},
		Tokens:      iss,
		OTPSender:   sender,
		Config:      Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5},
	}), sender
}

func TestSignupThenLogin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Signup(ctx, "a@b.com", "pw12345", "Alice")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected non-empty token pair")
	}

	if _, err := svc.Signup(ctx, "a@b.com", "pw", "Dup"); err == nil {
		t.Error("expected ErrAlreadyExists on duplicate email")
	}

	if _, err := svc.Login(ctx, "a@b.com", "pw12345"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := svc.Login(ctx, "a@b.com", "wrong"); err == nil {
		t.Error("expected error on wrong password")
	}
	if _, err := svc.Login(ctx, "nobody@b.com", "pw"); err == nil {
		t.Error("expected error on unknown email")
	}
}

func TestRefreshRotatesAndRevokes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Signup(ctx, "r@b.com", "pw12345", "R")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	rotated, err := svc.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == pair.RefreshToken {
		t.Error("refresh token should rotate")
	}
	// Old token must now be rejected (revoked).
	if _, err := svc.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Error("old refresh token must be rejected after rotation")
	}
	// Logout revokes the current one.
	if err := svc.Logout(ctx, rotated.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken); err == nil {
		t.Error("refresh after logout must fail")
	}
}
