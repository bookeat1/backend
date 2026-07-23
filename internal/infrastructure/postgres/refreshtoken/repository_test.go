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

func TestRevokeAllByUser(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "X", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	other := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: other, FullName: "Y", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed other user: %v", err)
	}

	repo := New(db)
	t1 := &domain.RefreshToken{ID: uuid.New(), UserID: uid, TokenHash: "th1", ExpiresAt: time.Now().Add(time.Hour)}
	t2 := &domain.RefreshToken{ID: uuid.New(), UserID: uid, TokenHash: "th2", ExpiresAt: time.Now().Add(time.Hour)}
	tOther := &domain.RefreshToken{ID: uuid.New(), UserID: other, TokenHash: "th-other", ExpiresAt: time.Now().Add(time.Hour)}
	for _, rt := range []*domain.RefreshToken{t1, t2, tOther} {
		if err := repo.Create(ctx, rt); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	if err := repo.RevokeAllByUser(ctx, uid); err != nil {
		t.Fatalf("RevokeAllByUser: %v", err)
	}
	for _, hash := range []string{"th1", "th2"} {
		got, err := repo.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash(%q): %v", hash, err)
		}
		if got.RevokedAt == nil {
			t.Errorf("token %q: expected RevokedAt to be set", hash)
		}
	}
	got, err := repo.GetByHash(ctx, "th-other")
	if err != nil {
		t.Fatalf("GetByHash(th-other): %v", err)
	}
	if got.RevokedAt != nil {
		t.Error("another user's token must not be revoked")
	}

	// Idempotent: calling it again with no live tokens left is a no-op success.
	if err := repo.RevokeAllByUser(ctx, uid); err != nil {
		t.Fatalf("second RevokeAllByUser: %v", err)
	}
}
