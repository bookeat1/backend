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
func (f *fakeUsers) Delete(_ context.Context, id uuid.UUID) error {
	u, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if u.DeletedAt != nil {
		return nil
	}
	now := time.Now()
	u.DeletedAt = &now
	u.Email, u.Phone, u.FullName, u.AvatarURL, u.City, u.CountryCode, u.BirthDate = nil, nil, "", nil, nil, nil, nil
	u.IsActive = false
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
func (f *fakeRefresh) RevokeAllByUser(_ context.Context, userID uuid.UUID) error {
	now := time.Now()
	for _, t := range f.byHash {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &now
		}
	}
	return nil
}

// fakeOTP is defined here; exercised in Task 12.
type fakeOTP struct {
	codes []*domain.OTPCode
	// lastUsedErr makes the delivery memory misbehave, so a test can prove the
	// usecase treats it as best effort and never lets it break a login.
	lastUsedErr error
}

func newFakeOTP() *fakeOTP { return &fakeOTP{} }
func (f *fakeOTP) Create(_ context.Context, c *domain.OTPCode) error {
	cp := *c
	f.codes = append(f.codes, &cp)
	return nil
}

// LatestActiveByPhone returns a COPY, like the Postgres repository does: a
// caller must not see its snapshot change under it when a later call writes to
// the row (that aliasing hid an off-by-one in the attempt counter once).
func (f *fakeOTP) LatestActiveByPhone(_ context.Context, phone string) (*domain.OTPCode, error) {
	for i := len(f.codes) - 1; i >= 0; i-- {
		c := f.codes[i]
		if c.Phone == phone && c.UsedAt == nil && c.ExpiresAt.After(time.Now()) {
			cp := *c
			return &cp, nil
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

// LastUsedChannelByPhone mirrors the SQL: the newest USED code for the phone
// whose channel is one a guest can be routed back to.
func (f *fakeOTP) LastUsedChannelByPhone(_ context.Context, phone string) (string, error) {
	if f.lastUsedErr != nil {
		return "", f.lastUsedErr
	}
	for i := len(f.codes) - 1; i >= 0; i-- {
		c := f.codes[i]
		if c.Phone == phone && c.UsedAt != nil && domain.OTPRememberableChannel(c.Channel) {
			return c.Channel, nil
		}
	}
	return "", nil
}

func (f *fakeOTP) InvalidateActiveByPhone(_ context.Context, phone string) error {
	now := time.Now()
	for _, c := range f.codes {
		if c.Phone == phone && c.UsedAt == nil && c.ExpiresAt.After(now) {
			c.UsedAt = &now
		}
	}
	return nil
}

// noTx runs fn directly (no real transaction) — fine for unit tests.
type noTx struct{}

func (noTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func (noTx) Detach(ctx context.Context) context.Context { return ctx }

// stubSender records the code and the ordering hint it was handed. It answers
// with channel "test" unless a test sets a real channel name, and fails every
// send once err is set (the "no channel took the code" path).
type stubSender struct {
	lastCode string
	lastHint domain.OTPSendHint
	channel  string
	err      error
	calls    int
}

func (s *stubSender) Send(_ context.Context, _, code string, hint domain.OTPSendHint) (string, error) {
	s.calls++
	s.lastHint = hint
	s.lastCode = code
	if s.err != nil {
		return "", s.err
	}
	if s.channel != "" {
		return s.channel, nil
	}
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
