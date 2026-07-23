package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type memUsers struct{ m map[uuid.UUID]*domain.User }

func (f *memUsers) Create(_ context.Context, u *domain.User) error { f.m[u.ID] = u; return nil }
func (f *memUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.m[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *memUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f *memUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f *memUsers) Update(_ context.Context, u *domain.User) error {
	if _, ok := f.m[u.ID]; !ok {
		return domain.ErrNotFound
	}
	f.m[u.ID] = u
	return nil
}
func (f *memUsers) Delete(_ context.Context, id uuid.UUID) error {
	u, ok := f.m[id]
	if !ok {
		return domain.ErrNotFound
	}
	if u.DeletedAt != nil {
		return nil
	}
	now := time.Now()
	u.DeletedAt = &now
	u.Email, u.Phone, u.FullName, u.AvatarURL = nil, nil, "", nil
	u.City, u.CountryCode, u.BirthDate = nil, nil, nil
	u.IsActive = false
	return nil
}

// memCuisines is an in-memory domain.UserCuisinePreferenceRepository.
type memCuisines struct{ m map[uuid.UUID][]uuid.UUID }

func newMemCuisines() *memCuisines { return &memCuisines{m: map[uuid.UUID][]uuid.UUID{}} }
func (f *memCuisines) ListCategoryIDs(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(f.m[userID]))
	copy(out, f.m[userID])
	return out, nil
}
func (f *memCuisines) Replace(_ context.Context, userID uuid.UUID, ids []uuid.UUID) error {
	cp := make([]uuid.UUID, len(ids))
	copy(cp, ids)
	f.m[userID] = cp
	return nil
}

// memRefresh is an in-memory domain.RefreshTokenRepository, tracking only what
// this package's tests need: whether RevokeAllByUser was called.
type memRefresh struct{ revokedFor map[uuid.UUID]int }

func newMemRefresh() *memRefresh                                         { return &memRefresh{revokedFor: map[uuid.UUID]int{}} }
func (f *memRefresh) Create(context.Context, *domain.RefreshToken) error { return nil }
func (f *memRefresh) GetByHash(context.Context, string) (*domain.RefreshToken, error) {
	return nil, domain.ErrNotFound
}
func (f *memRefresh) Revoke(context.Context, uuid.UUID) error { return nil }
func (f *memRefresh) RevokeAllByUser(_ context.Context, userID uuid.UUID) error {
	f.revokedFor[userID]++
	return nil
}

// memOTP is an in-memory domain.OTPRepository, tracking only what this
// package's tests need: whether InvalidateActiveByPhone was called.
type memOTP struct{ invalidatedFor map[string]int }

func newMemOTP() *memOTP                                        { return &memOTP{invalidatedFor: map[string]int{}} }
func (f *memOTP) Create(context.Context, *domain.OTPCode) error { return nil }
func (f *memOTP) LatestActiveByPhone(context.Context, string) (*domain.OTPCode, error) {
	return nil, domain.ErrNotFound
}
func (f *memOTP) MarkUsed(context.Context, uuid.UUID) error          { return nil }
func (f *memOTP) IncrementAttempts(context.Context, uuid.UUID) error { return nil }
func (f *memOTP) CountSince(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (f *memOTP) InvalidateActiveByPhone(_ context.Context, phone string) error {
	f.invalidatedFor[phone]++
	return nil
}

// noTx runs fn directly (no real transaction) — fine for unit tests.
type noTx struct{}

func (noTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (noTx) Detach(ctx context.Context) context.Context                         { return ctx }

func strp(s string) *string { return &s }

func newTestFacade(users *memUsers) (Facade, *memRefresh, *memOTP) {
	refresh := newMemRefresh()
	otp := newMemOTP()
	f := NewFacade(users, newMemCuisines(), refresh, otp, noTx{})
	return f, refresh, otp
}

func TestMeAndUpdate(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f, _, _ := newTestFacade(repo)
	ctx := context.Background()

	got, err := f.Me(ctx, id)
	if err != nil || got.FullName != "Old" {
		t.Fatalf("Me = %+v, %v", got, err)
	}

	birthDate := time.Date(1998, 5, 4, 0, 0, 0, 0, time.UTC)
	updated, err := f.UpdateMe(ctx, id, UpdateInput{
		FullName: strp("New"), City: strp("Almaty"),
		CountryCode: strp("KZ"), BirthDate: &birthDate,
	})
	if err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	if updated.FullName != "New" || updated.City == nil || *updated.City != "Almaty" {
		t.Errorf("update not applied: %+v", updated)
	}
	if updated.CountryCode == nil || *updated.CountryCode != "KZ" {
		t.Errorf("country_code not applied: %+v", updated)
	}
	if updated.BirthDate == nil || !updated.BirthDate.Equal(birthDate) {
		t.Errorf("birth_date not applied: %+v", updated)
	}
	if updated.PreferredLanguage != "ru" {
		t.Errorf("nil field should be unchanged, got %q", updated.PreferredLanguage)
	}

	if _, err := f.Me(ctx, uuid.New()); err == nil {
		t.Error("expected ErrNotFound for unknown user")
	}
}

func TestUpdateMeRejectsInvalidCountryCode(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f, _, _ := newTestFacade(repo)
	ctx := context.Background()

	for _, bad := range []string{"kz", "ZZ", "KAZ", "1Z", ""} {
		if _, err := f.UpdateMe(ctx, id, UpdateInput{CountryCode: strp(bad)}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("CountryCode=%q: err = %v, want ErrValidation", bad, err)
		}
	}
}

func TestUpdateMeRejectsBadBirthDate(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f, _, _ := newTestFacade(repo)
	ctx := context.Background()

	future := time.Now().AddDate(0, 0, 1)
	if _, err := f.UpdateMe(ctx, id, UpdateInput{BirthDate: &future}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("future birth_date: err = %v, want ErrValidation", err)
	}

	tooOld := time.Now().AddDate(-130, 0, 0)
	if _, err := f.UpdateMe(ctx, id, UpdateInput{BirthDate: &tooOld}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("birth_date 130 years ago: err = %v, want ErrValidation", err)
	}
}

func TestCuisinePreferencesRoundTrip(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f, _, _ := newTestFacade(repo)
	ctx := context.Background()

	catA, catB := uuid.New(), uuid.New()
	if _, err := f.UpdateMe(ctx, id, UpdateInput{CuisineCategoryIDs: &[]uuid.UUID{catA, catB}}); err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	got, err := f.CuisinePreferences(ctx, id)
	if err != nil {
		t.Fatalf("CuisinePreferences: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("CuisinePreferences = %v, want 2 entries", got)
	}

	// Omitting the field leaves preferences unchanged.
	if _, err := f.UpdateMe(ctx, id, UpdateInput{FullName: strp("Renamed")}); err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	got, _ = f.CuisinePreferences(ctx, id)
	if len(got) != 2 {
		t.Errorf("preferences changed on unrelated update: %v", got)
	}

	// An empty (non-nil) slice clears all preferences.
	empty := []uuid.UUID{}
	if _, err := f.UpdateMe(ctx, id, UpdateInput{CuisineCategoryIDs: &empty}); err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	got, _ = f.CuisinePreferences(ctx, id)
	if len(got) != 0 {
		t.Errorf("expected preferences cleared, got %v", got)
	}
}

func TestDeleteMeAnonymizesAndInvalidatesSessions(t *testing.T) {
	id := uuid.New()
	phone := "+77011234567"
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Alice", Phone: &phone, Email: strp("a@b.com"), Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru"},
	}}
	f, refresh, otp := newTestFacade(repo)
	ctx := context.Background()

	if err := f.DeleteMe(ctx, id); err != nil {
		t.Fatalf("DeleteMe: %v", err)
	}
	u, err := f.Me(ctx, id)
	if err != nil {
		t.Fatalf("Me after delete: %v", err)
	}
	if u.DeletedAt == nil {
		t.Error("expected DeletedAt to be set")
	}
	if u.FullName != "" || u.Phone != nil || u.Email != nil || u.IsActive {
		t.Errorf("expected anonymized user, got %+v", u)
	}
	if refresh.revokedFor[id] != 1 {
		t.Errorf("expected RevokeAllByUser called once, got %d", refresh.revokedFor[id])
	}
	if otp.invalidatedFor[phone] != 1 {
		t.Errorf("expected InvalidateActiveByPhone called once for %q, got %d", phone, otp.invalidatedFor[phone])
	}

	// Idempotent: a second call succeeds and does not re-run session
	// invalidation (nothing left to invalidate, and re-anonymizing would be a
	// no-op anyway).
	if err := f.DeleteMe(ctx, id); err != nil {
		t.Fatalf("second DeleteMe: %v", err)
	}
	if refresh.revokedFor[id] != 1 {
		t.Errorf("expected RevokeAllByUser still called once after idempotent retry, got %d", refresh.revokedFor[id])
	}
}

func TestDeleteMeUnknownUserIsNotFound(t *testing.T) {
	repo := &memUsers{m: map[uuid.UUID]*domain.User{}}
	f, _, _ := newTestFacade(repo)
	if err := f.DeleteMe(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteMe unknown user = %v, want ErrNotFound", err)
	}
}
