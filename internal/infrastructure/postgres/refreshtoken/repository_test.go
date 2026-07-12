package refreshtoken

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
)

func TestCreateGetRevoke(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "X", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	repo := New(db)
	rt := &domain.RefreshToken{ID: uuid.New(), UserID: uid, TokenHash: "th", ExpiresAt: time.Now().Add(time.Hour)}
	if err := repo.Create(ctx, rt); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.GetByHash(ctx, "th")
	if err != nil || got.ID != rt.ID {
		t.Fatalf("GetByHash = %+v, %v", got, err)
	}
	if err := repo.Revoke(ctx, rt.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ = repo.GetByHash(ctx, "th")
	if got.RevokedAt == nil {
		t.Error("expected RevokedAt to be set after Revoke")
	}
	if _, err := repo.GetByHash(ctx, "missing"); err == nil {
		t.Error("expected ErrNotFound for unknown hash")
	}
}
