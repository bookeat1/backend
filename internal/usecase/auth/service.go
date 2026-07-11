package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/password"
	"backend-core/internal/domain"
)

// dummyPasswordHash is a valid bcrypt hash used only to equalize Login timing on
// the user-not-found / no-credential branches, so response latency does not
// reveal whether an email exists (defeats a timing-based enumeration oracle).
const dummyPasswordHash = "$2a$10$agfc3jmOzd2VFYd88HzJwe7fzRu49fXBTFt3GKrOoV780qxbiYEKC"

// hashOpaque returns the sha256 hex of an opaque token (refresh tokens are
// stored hashed, never in the clear).
func hashOpaque(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a URL-safe 256-bit random string.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// issuePair issues an access JWT and a fresh rotating refresh token for user.
func (s *Service) issuePair(ctx context.Context, u *domain.User) (*TokenPair, error) {
	access, exp, err := s.d.Tokens.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		return nil, fmt.Errorf("issue access: %w", err)
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	rt := &domain.RefreshToken{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: hashOpaque(refresh),
		ExpiresAt: time.Now().Add(s.d.Config.RefreshTTL),
	}
	if err := s.d.Refresh.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh: %w", err)
	}
	return &TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp}, nil
}

// Signup creates a password user and returns a token pair. ErrAlreadyExists if
// the email is taken.
func (s *Service) Signup(ctx context.Context, email, pw, fullName string) (*TokenPair, error) {
	if email == "" || pw == "" {
		return nil, fmt.Errorf("%w: email and password required", domain.ErrValidation)
	}
	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := s.d.Users.GetByEmail(ctx, email); err == nil {
			return fmt.Errorf("%w: email", domain.ErrAlreadyExists)
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		hash, err := password.Hash(pw)
		if err != nil {
			return err
		}
		u := &domain.User{ID: uuid.New(), Email: &email, FullName: fullName, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru"}
		if err := s.d.Users.Create(ctx, u); err != nil {
			return err
		}
		if err := s.d.Credentials.Upsert(ctx, &domain.UserCredential{UserID: u.ID, PasswordHash: hash}); err != nil {
			return err
		}
		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Login verifies an email/password and returns a token pair. Returns
// ErrUnauthorized on any mismatch (no user-enumeration signal).
func (s *Service) Login(ctx context.Context, email, pw string) (*TokenPair, error) {
	u, err := s.d.Users.GetByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		password.Verify(dummyPasswordHash, pw)
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	cred, err := s.d.Credentials.GetByUserID(ctx, u.ID)
	if errors.Is(err, domain.ErrNotFound) {
		password.Verify(dummyPasswordHash, pw)
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !password.Verify(cred.PasswordHash, pw) {
		return nil, domain.ErrUnauthorized
	}
	return s.issuePair(ctx, u)
}

// Refresh validates a refresh token, rotates it (revokes the old, issues a new
// pair). Rejects unknown, expired, revoked, or reused tokens.
func (s *Service) Refresh(ctx context.Context, refresh string) (*TokenPair, error) {
	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		rt, err := s.d.Refresh.GetByHash(ctx, hashOpaque(refresh))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil || time.Now().After(rt.ExpiresAt) {
			return domain.ErrUnauthorized
		}
		if err := s.d.Refresh.Revoke(ctx, rt.ID); err != nil {
			return err
		}
		u, err := s.d.Users.GetByID(ctx, rt.UserID)
		if err != nil {
			return err
		}
		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes the given refresh token. Unknown tokens are a no-op success.
func (s *Service) Logout(ctx context.Context, refresh string) error {
	rt, err := s.d.Refresh.GetByHash(ctx, hashOpaque(refresh))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.d.Refresh.Revoke(ctx, rt.ID)
}
