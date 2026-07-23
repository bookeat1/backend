package usercuisine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

func TestListAndReplace(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "Foodie", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	catA, catB, catC := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{catA, catB, catC} {
		if _, err := db.Exec(ctx, `INSERT INTO restaurant_categories (id, name) VALUES ($1, 'Cat')`, id); err != nil {
			t.Fatalf("seed category: %v", err)
		}
	}

	repo := New(db)

	got, err := repo.ListCategoryIDs(ctx, uid)
	if err != nil {
		t.Fatalf("ListCategoryIDs (empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no preferences yet, got %v", got)
	}

	if err := repo.Replace(ctx, uid, []uuid.UUID{catA, catB}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err = repo.ListCategoryIDs(ctx, uid)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListCategoryIDs = %v, %v, want 2 entries", got, err)
	}

	// Replace again with a different set: old picks are gone, new ones present.
	if err := repo.Replace(ctx, uid, []uuid.UUID{catC}); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	got, err = repo.ListCategoryIDs(ctx, uid)
	if err != nil || len(got) != 1 || got[0] != catC {
		t.Fatalf("ListCategoryIDs after replace = %v, %v, want [%v]", got, err, catC)
	}

	// Replace with an empty slice clears everything.
	if err := repo.Replace(ctx, uid, nil); err != nil {
		t.Fatalf("Replace with nil: %v", err)
	}
	got, err = repo.ListCategoryIDs(ctx, uid)
	if err != nil || len(got) != 0 {
		t.Fatalf("ListCategoryIDs after clear = %v, %v, want empty", got, err)
	}
}

// TestReplaceRejectsUnknownCategoryInsideATx exercises Replace the way its
// only real caller (usecase/users.UpdateMe) does: inside a
// domain.TxManager.WithinTx. A bad id among the requested set must fail the
// whole call AND roll the delete back, so a non-empty previous pick set
// survives untouched.
func TestReplaceRejectsUnknownCategoryInsideATx(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "Foodie", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	goodCat := uuid.New()
	if _, err := db.Exec(ctx, `INSERT INTO restaurant_categories (id, name) VALUES ($1, 'Cat')`, goodCat); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	repo := New(db)
	if err := repo.Replace(ctx, uid, []uuid.UUID{goodCat}); err != nil {
		t.Fatalf("seed initial preference: %v", err)
	}

	txm := sqltx.NewManager(db)
	unknownCat := uuid.New()
	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return repo.Replace(ctx, uid, []uuid.UUID{unknownCat})
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Replace with unknown category = %v, want ErrValidation", err)
	}

	got, err := repo.ListCategoryIDs(ctx, uid)
	if err != nil || len(got) != 1 || got[0] != goodCat {
		t.Fatalf("ListCategoryIDs after rolled-back Replace = %v, %v, want [%v] (previous set intact)", got, err, goodCat)
	}
}
