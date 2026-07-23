package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
)

// fakeUsers is an in-memory domain.UserRepository.
type fakeUsers struct{ byID map[uuid.UUID]*domain.User }

func newFakeUsers() *fakeUsers { return &fakeUsers{byID: map[uuid.UUID]*domain.User{}} }

func (f *fakeUsers) Create(_ context.Context, u *domain.User) error {
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}
func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	for _, u := range f.byID {
		if u.Email != nil && *u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	for _, u := range f.byID {
		if u.Phone != nil && *u.Phone == phone {
			cp := *u
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) Update(_ context.Context, u *domain.User) error {
	if _, ok := f.byID[u.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}

type fakeCreds struct{ byUser map[uuid.UUID]string }

func newFakeCreds() *fakeCreds { return &fakeCreds{byUser: map[uuid.UUID]string{}} }
func (f *fakeCreds) Upsert(_ context.Context, c *domain.UserCredential) error {
	f.byUser[c.UserID] = c.PasswordHash
	return nil
}
func (f *fakeCreds) GetByUserID(_ context.Context, id uuid.UUID) (*domain.UserCredential, error) {
	if h, ok := f.byUser[id]; ok {
		return &domain.UserCredential{UserID: id, PasswordHash: h}, nil
	}
	return nil, domain.ErrNotFound
}

type fakeRefresh struct {
	byHash map[string]*domain.RefreshToken
}

func newFakeRefresh() *fakeRefresh { return &fakeRefresh{byHash: map[string]*domain.RefreshToken{}} }
func (f *fakeRefresh) Create(_ context.Context, t *domain.RefreshToken) error {
	cp := *t
	f.byHash[t.TokenHash] = &cp
	return nil
}
func (f *fakeRefresh) GetByHash(_ context.Context, h string) (*domain.RefreshToken, error) {
	if t, ok := f.byHash[h]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeRefresh) Revoke(_ context.Context, id uuid.UUID) error {
	for _, t := range f.byHash {
		if t.ID == id {
			now := time.Now()
			t.RevokedAt = &now
		}
	}
	return nil
}

// fakeOTP is defined here; exercised in Task 12.
type fakeOTP struct{ codes []*domain.OTPCode }

func newFakeOTP() *fakeOTP { return &fakeOTP{} }
func (f *fakeOTP) Create(_ context.Context, c *domain.OTPCode) error {
	cp := *c
	f.codes = append(f.codes, &cp)
	return nil
}
func (f *fakeOTP) LatestActiveByPhone(_ context.Context, phone string) (*domain.OTPCode, error) {
	for i := len(f.codes) - 1; i >= 0; i-- {
		c := f.codes[i]
		if c.Phone == phone && c.UsedAt == nil && c.ExpiresAt.After(time.Now()) {
			return c, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeOTP) MarkUsed(_ context.Context, id uuid.UUID) error {
	for _, c := range f.codes {
		if c.ID == id {
			now := time.Now()
			c.UsedAt = &now
		}
	}
	return nil
}
func (f *fakeOTP) IncrementAttempts(_ context.Context, id uuid.UUID) error {
	for _, c := range f.codes {
		if c.ID == id {
			c.Attempts++
		}
	}
	return nil
}
func (f *fakeOTP) CountSince(_ context.Context, phone string, ts time.Time) (int, error) {
	n := 0
	for _, c := range f.codes {
		if c.Phone == phone && !c.CreatedAt.Before(ts) {
			n++
		}
	}
	return n, nil
}

// noTx runs fn directly (no real transaction) — fine for unit tests.
type noTx struct{}

func (noTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func (noTx) Detach(ctx context.Context) context.Context { return ctx }

// stubSender records nothing and returns channel "test".
type stubSender struct{ lastCode string }

func (s *stubSender) Send(_ context.Context, _, code string) (string, error) {
	s.lastCode = code
	return "test", nil
}

// testIssuer builds a real RSAIssuer for tests via the token package helper.
func testIssuer(t *testing.T) TokenIssuer {
	t.Helper()
	iss, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	return iss
}
