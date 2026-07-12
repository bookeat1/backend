package user

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func strp(s string) *string { return &s }

func TestCreateGetUpdate(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	u := &domain.User{ID: uuid.New(), Email: strp("a@b.com"), FullName: "Alice", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FullName != "Alice" || got.Email == nil || *got.Email != "a@b.com" {
		t.Errorf("unexpected user: %+v", got)
	}

	byEmail, err := repo.GetByEmail(ctx, "a@b.com")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("GetByEmail: %v / %+v", err, byEmail)
	}

	u.FullName = "Alice B"
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = repo.GetByID(ctx, u.ID)
	if got.FullName != "Alice B" {
		t.Errorf("update not persisted: %q", got.FullName)
	}
}

func TestCreateDuplicateEmailMapsToAlreadyExists(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	first := &domain.User{ID: uuid.New(), Email: strp("dup@b.com"), FullName: "First", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Same email, different id: the UNIQUE constraint fires and must surface as
	// ErrAlreadyExists (SQLSTATE 23505 mapping), not a raw error.
	second := &domain.User{ID: uuid.New(), Email: strp("dup@b.com"), FullName: "Second", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, second); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate email Create = %v, want ErrAlreadyExists", err)
	}
}

func TestGetByIDNotFound(t *testing.T) {
	db := testdb.Connect(t)
	repo := New(db)
	if _, err := repo.GetByID(context.Background(), uuid.New()); err == nil {
		t.Error("expected ErrNotFound")
	}
}
