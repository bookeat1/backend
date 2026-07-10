package user

import (
	"context"
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

func TestGetByIDNotFound(t *testing.T) {
	db := testdb.Connect(t)
	repo := New(db)
	if _, err := repo.GetByID(context.Background(), uuid.New()); err == nil {
		t.Error("expected ErrNotFound")
	}
}
