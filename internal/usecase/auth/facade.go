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

// Facade is the core credential/session authentication usecase: email+password
// signup and login, JWT issuance, and refresh-token rotation. Phone-OTP login is
// a separate concern, exposed by OTPUseCase (otp.go).
type Facade interface {
	Signup(ctx context.Context, email, pw, fullName string) (*TokenPair, error)
	Login(ctx context.Context, email, pw string) (*TokenPair, error)
	Refresh(ctx context.Context, refresh string) (*TokenPair, error)
	Logout(ctx context.Context, refresh string) error
}

type facade struct {
	users   domain.UserRepository
	creds   domain.UserCredentialRepository
	refresh domain.RefreshTokenRepository
	tx      domain.TxManager
	tokens  TokenIssuer
	cfg     Config
}

// NewFacade constructs the core auth Facade.
func NewFacade(
	users domain.UserRepository,
	creds domain.UserCredentialRepository,
	refresh domain.RefreshTokenRepository,
	tx domain.TxManager,
	tokens TokenIssuer,
	cfg Config,
) Facade {
	return &facade{users: users, creds: creds, refresh: refresh, tx: tx, tokens: tokens, cfg: cfg}
}

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

// issuePair issues an access JWT and a fresh rotating refresh token for u. It is
// shared by the Facade and the OTPUseCase, so it is a free function over the
// minimal set of dependencies it needs rather than a method.
func issuePair(
	ctx context.Context,
	tokens TokenIssuer,
	refresh domain.RefreshTokenRepository,
	refreshTTL time.Duration,
	u *domain.User,
) (*TokenPair, error) {
	access, exp, err := tokens.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		return nil, fmt.Errorf("issue access: %w", err)
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	rt := &domain.RefreshToken{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: hashOpaque(tok),
		ExpiresAt: time.Now().Add(refreshTTL),
	}
	if err := refresh.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh: %w", err)
	}
	return &TokenPair{AccessToken: access, RefreshToken: tok, ExpiresAt: exp}, nil
}

// Signup creates a password user and returns a token pair. ErrAlreadyExists if
// the email is taken.
func (f *facade) Signup(ctx context.Context, email, pw, fullName string) (*TokenPair, error) {
	if email == "" || pw == "" {
		return nil, fmt.Errorf("%w: email and password required", domain.ErrValidation)
	}
	var pair *TokenPair
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := f.users.GetByEmail(ctx, email); err == nil {
			return fmt.Errorf("%w: email", domain.ErrAlreadyExists)
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		hash, err := password.Hash(pw)
		if err != nil {
			return err
		}
		u := &domain.User{ID: uuid.New(), Email: &email, FullName: fullName, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru"}
		if err := f.users.Create(ctx, u); err != nil {
			return err
		}
		if err := f.creds.Upsert(ctx, &domain.UserCredential{UserID: u.ID, PasswordHash: hash}); err != nil {
			return err
		}
		pair, err = issuePair(ctx, f.tokens, f.refresh, f.cfg.RefreshTTL, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Login verifies an email/password and returns a token pair. Returns
// ErrUnauthorized on any mismatch (no user-enumeration signal).
func (f *facade) Login(ctx context.Context, email, pw string) (*TokenPair, error) {
	u, err := f.users.GetByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		password.Verify(dummyPasswordHash, pw)
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	cred, err := f.creds.GetByUserID(ctx, u.ID)
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
	return issuePair(ctx, f.tokens, f.refresh, f.cfg.RefreshTTL, u)
}

// Refresh validates a refresh token, rotates it (revokes the old, issues a new
// pair). Rejects unknown, expired, revoked, or reused tokens.
func (f *facade) Refresh(ctx context.Context, refresh string) (*TokenPair, error) {
	var pair *TokenPair
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		rt, err := f.refresh.GetByHash(ctx, hashOpaque(refresh))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil || time.Now().After(rt.ExpiresAt) {
			return domain.ErrUnauthorized
		}
		if err := f.refresh.Revoke(ctx, rt.ID); err != nil {
			return err
		}
		u, err := f.users.GetByID(ctx, rt.UserID)
		if err != nil {
			return err
		}
		pair, err = issuePair(ctx, f.tokens, f.refresh, f.cfg.RefreshTTL, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes the given refresh token. Unknown tokens are a no-op success.
func (f *facade) Logout(ctx context.Context, refresh string) error {
	rt, err := f.refresh.GetByHash(ctx, hashOpaque(refresh))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.refresh.Revoke(ctx, rt.ID)
}
