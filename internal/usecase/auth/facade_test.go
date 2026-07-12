package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// newTestFacade wires the core auth Facade over in-memory fakes.
func newTestFacade(t *testing.T) Facade {
	t.Helper()
	return NewFacade(newFakeUsers(), newFakeCreds(), newFakeRefresh(), noTx{}, testIssuer(t), Config{RefreshTTL: time.Hour})
}

func TestSignupThenLogin(t *testing.T) {
	svc := newTestFacade(t)
	ctx := context.Background()

	pair, err := svc.Signup(ctx, "a@b.com", "pw12345", "Alice")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected non-empty token pair")
	}

	if _, err := svc.Signup(ctx, "a@b.com", "pw", "Dup"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists on duplicate email, got %v", err)
	}

	if _, err := svc.Login(ctx, "a@b.com", "pw12345"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := svc.Login(ctx, "a@b.com", "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized on wrong password, got %v", err)
	}
	if _, err := svc.Login(ctx, "nobody@b.com", "pw"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized on unknown email, got %v", err)
	}
}

func TestRefreshRotatesAndRevokes(t *testing.T) {
	svc := newTestFacade(t)
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
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("old refresh token must be rejected after rotation with ErrUnauthorized, got %v", err)
	}
	// Logout revokes the current one.
	if err := svc.Logout(ctx, rotated.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("refresh after logout must fail with ErrUnauthorized, got %v", err)
	}
}
