package usercuisine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

func TestListAndReplace(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	seedCuisines(t, db)
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "Foodie", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	catA, catB, catC := seedCuisine(t, db, "a"), seedCuisine(t, db, "b"), seedCuisine(t, db, "c")

	repo := New(db)

	got, err := repo.ListCuisineIDs(ctx, uid)
	if err != nil {
		t.Fatalf("ListCuisineIDs (empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no preferences yet, got %v", got)
	}

	if err := repo.Replace(ctx, uid, []uuid.UUID{catA, catB}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err = repo.ListCuisineIDs(ctx, uid)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListCuisineIDs = %v, %v, want 2 entries", got, err)
	}

	// Replace again with a different set: old picks are gone, new ones present.
	if err := repo.Replace(ctx, uid, []uuid.UUID{catC}); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	got, err = repo.ListCuisineIDs(ctx, uid)
	if err != nil || len(got) != 1 || got[0] != catC {
		t.Fatalf("ListCuisineIDs after replace = %v, %v, want [%v]", got, err, catC)
	}

	// Replace with an empty slice clears everything.
	if err := repo.Replace(ctx, uid, nil); err != nil {
		t.Fatalf("Replace with nil: %v", err)
	}
	got, err = repo.ListCuisineIDs(ctx, uid)
	if err != nil || len(got) != 0 {
		t.Fatalf("ListCuisineIDs after clear = %v, %v, want empty", got, err)
	}
}

// TestReplaceRejectsUnknownCuisineInsideATx exercises Replace the way its
// only real caller (usecase/users.UpdateMe) does: inside a
// domain.TxManager.WithinTx. A bad id among the requested set must fail the
// whole call AND roll the delete back, so a non-empty previous pick set
// survives untouched.
func TestReplaceRejectsUnknownCuisineInsideATx(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	seedCuisines(t, db)
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "Foodie", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	goodCat := seedCuisine(t, db, "good")

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
		t.Fatalf("Replace with unknown cuisine = %v, want ErrValidation", err)
	}

	got, err := repo.ListCuisineIDs(ctx, uid)
	if err != nil || len(got) != 1 || got[0] != goodCat {
		t.Fatalf("ListCuisineIDs after rolled-back Replace = %v, %v, want [%v] (previous set intact)", got, err, goodCat)
	}
}

// seedCuisines clears the dictionary so a test starts from a known state.
// restaurant_cuisines hangs off it with an ON DELETE RESTRICT foreign key, so
// the cascade has to name that table too.
func seedCuisines(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	testdb.Truncate(t, db, "restaurant_cuisines", "cuisine_aliases", "cuisines")
}

// seedCuisine inserts one dictionary entry and returns its id.
func seedCuisine(t *testing.T, db *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO cuisines (id, code, name) VALUES ($1,$2,$3)`, id, code, "Cuisine "+code); err != nil {
		t.Fatalf("seed cuisine %s: %v", code, err)
	}
	return id
}
