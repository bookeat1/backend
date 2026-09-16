package foodieprofile

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
)

func TestGetAndReplace(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "Foodie", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	repo := New(db)

	got, err := repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get (empty): %v", err)
	}
	if len(got.Cuisines) != 0 || len(got.Diets) != 0 || len(got.Allergies) != 0 {
		t.Fatalf("expected empty profile for a new user, got %+v", got)
	}

	prefs := domain.FoodieProfilePreferences{
		Cuisines:  []string{domain.FoodieCuisineKazakh, domain.FoodieCuisineAsian},
		Diets:     []string{domain.FoodieDietHalal},
		Allergies: []string{domain.FoodieAllergyNuts, domain.FoodieAllergyDairy},
	}
	if err := repo.Replace(ctx, uid, prefs); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err = repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get after replace: %v", err)
	}
	if len(got.Cuisines) != 2 || len(got.Diets) != 1 || len(got.Allergies) != 2 {
		t.Fatalf("Get after replace = %+v, want 2/1/2 entries", got)
	}

	// Replace again with a different, smaller set: old picks in every table
	// are fully gone, only the new ones remain — proves DELETE-all runs on
	// every axis, not just the ones present in the new call.
	second := domain.FoodieProfilePreferences{
		Cuisines: []string{domain.FoodieCuisineVegan},
	}
	if err := repo.Replace(ctx, uid, second); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	got, err = repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get after second replace: %v", err)
	}
	if len(got.Cuisines) != 1 || got.Cuisines[0] != domain.FoodieCuisineVegan {
		t.Errorf("cuisines after second replace = %v, want [%s]", got.Cuisines, domain.FoodieCuisineVegan)
	}
	if len(got.Diets) != 0 {
		t.Errorf("diets after second replace = %v, want empty (fully cleared)", got.Diets)
	}
	if len(got.Allergies) != 0 {
		t.Errorf("allergies after second replace = %v, want empty (fully cleared)", got.Allergies)
	}

	// Replace with an empty struct clears everything — idempotent on repeat.
	if err := repo.Replace(ctx, uid, domain.FoodieProfilePreferences{}); err != nil {
		t.Fatalf("clearing Replace: %v", err)
	}
	if err := repo.Replace(ctx, uid, domain.FoodieProfilePreferences{}); err != nil {
		t.Fatalf("second clearing Replace (idempotent): %v", err)
	}
	got, err = repo.Get(ctx, uid)
	if err != nil || len(got.Cuisines) != 0 || len(got.Diets) != 0 || len(got.Allergies) != 0 {
		t.Fatalf("Get after clear = %+v, %v, want all empty", got, err)
	}
}

func TestReplaceIsScopedPerUser(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	users := userrepo.New(db)
	uidA, uidB := uuid.New(), uuid.New()
	if err := users.Create(ctx, &domain.User{ID: uidA, FullName: "A", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user A: %v", err)
	}
	if err := users.Create(ctx, &domain.User{ID: uidB, FullName: "B", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user B: %v", err)
	}

	repo := New(db)
	if err := repo.Replace(ctx, uidA, domain.FoodieProfilePreferences{Cuisines: []string{domain.FoodieCuisineKazakh}}); err != nil {
		t.Fatalf("Replace A: %v", err)
	}
	if err := repo.Replace(ctx, uidB, domain.FoodieProfilePreferences{Cuisines: []string{domain.FoodieCuisineItalian}}); err != nil {
		t.Fatalf("Replace B: %v", err)
	}

	gotA, _ := repo.Get(ctx, uidA)
	gotB, _ := repo.Get(ctx, uidB)
	if len(gotA.Cuisines) != 1 || gotA.Cuisines[0] != domain.FoodieCuisineKazakh {
		t.Errorf("user A cuisines = %v, want [%s]", gotA.Cuisines, domain.FoodieCuisineKazakh)
	}
	if len(gotB.Cuisines) != 1 || gotB.Cuisines[0] != domain.FoodieCuisineItalian {
		t.Errorf("user B cuisines = %v, want [%s]", gotB.Cuisines, domain.FoodieCuisineItalian)
	}
}
