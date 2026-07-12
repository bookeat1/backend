package users

import (
	"context"
	"testing"

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

func strp(s string) *string { return &s }

func TestMeAndUpdate(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f := NewFacade(repo)
	ctx := context.Background()

	got, err := f.Me(ctx, id)
	if err != nil || got.FullName != "Old" {
		t.Fatalf("Me = %+v, %v", got, err)
	}

	updated, err := f.UpdateMe(ctx, id, UpdateInput{FullName: strp("New"), City: strp("Almaty")})
	if err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	if updated.FullName != "New" || updated.City == nil || *updated.City != "Almaty" {
		t.Errorf("update not applied: %+v", updated)
	}
	if updated.PreferredLanguage != "ru" {
		t.Errorf("nil field should be unchanged, got %q", updated.PreferredLanguage)
	}

	if _, err := f.Me(ctx, uuid.New()); err == nil {
		t.Error("expected ErrNotFound for unknown user")
	}
}
