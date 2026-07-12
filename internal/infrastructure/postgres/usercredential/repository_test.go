package usercredential

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
)

func TestUpsertAndGet(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	id := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: id, FullName: "X", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	repo := New(db)
	if err := repo.Upsert(ctx, &domain.UserCredential{UserID: id, PasswordHash: "h1"}); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	if err := repo.Upsert(ctx, &domain.UserCredential{UserID: id, PasswordHash: "h2"}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, err := repo.GetByUserID(ctx, id)
	if err != nil || got.PasswordHash != "h2" {
		t.Fatalf("GetByUserID = %+v, %v", got, err)
	}
	if _, err := repo.GetByUserID(ctx, uuid.New()); err == nil {
		t.Error("expected ErrNotFound for unknown user")
	}
}
