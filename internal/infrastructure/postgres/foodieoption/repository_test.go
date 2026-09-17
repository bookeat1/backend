package foodieoption

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	cuisinerepo "backend-core/internal/infrastructure/postgres/cuisine"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func priceCat(p domain.PriceCategory) *domain.PriceCategory { return &p }

func TestListOrderingAndActiveFilter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	active := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindDiet, Code: "z_active", Name: "Z Active", DisplayOrder: 10, IsActive: true}
	hidden := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindDiet, Code: "a_hidden", Name: "A Hidden", DisplayOrder: 5, IsActive: false}
	if err := repo.Create(ctx, active); err != nil {
		t.Fatalf("create active: %v", err)
	}
	if err := repo.Create(ctx, hidden); err != nil {
		t.Fatalf("create hidden: %v", err)
	}

	got, err := repo.List(ctx, domain.FoodieOptionFilter{})
	if err != nil {
		t.Fatalf("List (public): %v", err)
	}
	if len(got) != 1 || got[0].Code != "z_active" {
		t.Fatalf("List (public) = %v, want only z_active", codesOf(got))
	}

	all, err := repo.List(ctx, domain.FoodieOptionFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("List (admin): %v", err)
	}
	// display_order 5 (hidden) sorts before 10 (active) — order does not care
	// about is_active.
	if len(all) != 2 || all[0].Code != "a_hidden" || all[1].Code != "z_active" {
		t.Fatalf("List (admin) = %v, want [a_hidden, z_active] in display_order", codesOf(all))
	}

	kind := domain.FoodieOptionKindCuisine
	none, err := repo.List(ctx, domain.FoodieOptionFilter{Kind: &kind, IncludeInactive: true})
	if err != nil {
		t.Fatalf("List (kind=cuisine): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("List (kind=cuisine) = %v, want empty (only diets seeded)", codesOf(none))
	}
}

func codesOf(items []domain.FoodieOption) []string {
	out := make([]string, len(items))
	for i, o := range items {
		out[i] = o.Code
	}
	return out
}

func TestCreateDuplicateCodeAndName(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	first := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindAllergy, Code: "nuts", Name: "Орехи", IsActive: true}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}

	dupCode := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindAllergy, Code: "nuts", Name: "Другое название", IsActive: true}
	if err := repo.Create(ctx, dupCode); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create dup code = %v, want ErrAlreadyExists", err)
	}

	// Same code is fine in a DIFFERENT kind (spec 3.8/6.12: "seafood" is both
	// a cuisine and an allergy today).
	otherKind := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindCuisine, Code: "nuts", Name: "Ореховая", IsActive: true}
	if err := repo.Create(ctx, otherKind); err != nil {
		t.Fatalf("create same code, different kind: %v", err)
	}

	// Duplicate name, case/whitespace-insensitive, same kind.
	dupName := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindAllergy, Code: "nuts2", Name: "  орехи ", IsActive: true}
	if err := repo.Create(ctx, dupName); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create dup name = %v, want ErrAlreadyExists", err)
	}
}

func TestUpdateAndCuisineLinks(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options", "cuisines")
	ctx := context.Background()
	repo := New(pool)
	cuisines := cuisinerepo.New(pool)

	c1 := &domain.Cuisine{ID: uuid.New(), Code: "korean", Name: "Корейская", IsActive: true, DisplayOrder: 1}
	c2 := &domain.Cuisine{ID: uuid.New(), Code: "thai", Name: "Тайская", IsActive: true, DisplayOrder: 2}
	if err := cuisines.Create(ctx, c1); err != nil {
		t.Fatalf("seed cuisine 1: %v", err)
	}
	if err := cuisines.Create(ctx, c2); err != nil {
		t.Fatalf("seed cuisine 2: %v", err)
	}

	opt := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindCuisine, Code: "korean", Name: "Корейская", IsActive: true}
	if err := repo.Create(ctx, opt); err != nil {
		t.Fatalf("create option: %v", err)
	}
	if err := repo.SetCuisineLinks(ctx, opt.ID, []uuid.UUID{c1.ID}); err != nil {
		t.Fatalf("set links: %v", err)
	}

	got, err := repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Cuisines) != 1 || got.Cuisines[0].Code != "korean" {
		t.Fatalf("GetByID cuisines = %v, want [korean]", got.Cuisines)
	}
	if !got.AffectsMatching() {
		t.Error("AffectsMatching() = false with a cuisine link, want true")
	}

	// Replace wholesale with a different set — the old link is GONE, not
	// merged (same convention as CuisineRepository.SetForRestaurant).
	if err := repo.SetCuisineLinks(ctx, opt.ID, []uuid.UUID{c2.ID}); err != nil {
		t.Fatalf("replace links: %v", err)
	}
	got, err = repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID after replace: %v", err)
	}
	if len(got.Cuisines) != 1 || got.Cuisines[0].Code != "thai" {
		t.Fatalf("GetByID cuisines after replace = %v, want [thai]", got.Cuisines)
	}

	// Clearing the set entirely: no cuisine link left, AffectsMatching flips.
	if err := repo.SetCuisineLinks(ctx, opt.ID, nil); err != nil {
		t.Fatalf("clear links: %v", err)
	}
	got, err = repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if len(got.Cuisines) != 0 {
		t.Fatalf("GetByID cuisines after clear = %v, want empty", got.Cuisines)
	}
	if got.AffectsMatching() {
		t.Error("AffectsMatching() = true with no cuisine link, want false")
	}

	// Update rewrites the base columns; is_active/name/description round-trip.
	desc := "desc"
	got.Name = "Корейская новая"
	got.IsActive = false
	got.Description = &desc
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, err := repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if reread.Name != "Корейская новая" || reread.IsActive || reread.Description == nil || *reread.Description != "desc" {
		t.Fatalf("GetByID after update = %+v, want updated fields", reread)
	}
}

func TestBudgetPriceCategoryRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	opt := &domain.FoodieOption{
		ID: uuid.New(), Kind: domain.FoodieOptionKindBudget, Code: "ultra", Name: "Ultra",
		PriceCategory: priceCat(domain.PriceHigh), IsActive: true,
	}
	if err := repo.Create(ctx, opt); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PriceCategory == nil || *got.PriceCategory != domain.PriceHigh {
		t.Fatalf("PriceCategory = %v, want %q", got.PriceCategory, domain.PriceHigh)
	}
	if !got.AffectsMatching() {
		t.Error("AffectsMatching() = false with a price_category set, want true")
	}

	// Clearing it back to nil ("не задан", spec 3.10) must round-trip too.
	got.PriceCategory = nil
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("clear price_category: %v", err)
	}
	reread, err := repo.GetByID(ctx, opt.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if reread.PriceCategory != nil {
		t.Fatalf("PriceCategory after clear = %v, want nil", reread.PriceCategory)
	}
	if reread.AffectsMatching() {
		t.Error("AffectsMatching() = true with no price_category, want false")
	}
}

// TestCreateAndUpdateReturnRealTimestamps pins the fix for the review bug
// where Create/Update only did RETURNING id, so the caller's o.CreatedAt/
// o.UpdatedAt stayed at their Go zero value ("0001-01-01T00:00:00Z") instead
// of the row's real now() — the admin POST/PATCH response echoed a bogus
// timestamp instead of what Postgres actually wrote.
func TestCreateAndUpdateReturnRealTimestamps(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	opt := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindDiet, Code: "ts_check", Name: "TS Check", IsActive: true}
	if err := repo.Create(ctx, opt); err != nil {
		t.Fatalf("create: %v", err)
	}
	if opt.CreatedAt.IsZero() {
		t.Fatal("Create left CreatedAt at the zero value, want the row's real created_at")
	}
	if opt.UpdatedAt.IsZero() {
		t.Fatal("Create left UpdatedAt at the zero value, want the row's real updated_at")
	}
	createdAt, updatedAtBefore := opt.CreatedAt, opt.UpdatedAt

	// A real clock tick between Create and Update so a naive "same instant"
	// implementation would still be caught.
	time.Sleep(10 * time.Millisecond)

	opt.Name = "TS Check Renamed"
	if err := repo.Update(ctx, opt); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !opt.CreatedAt.Equal(createdAt) {
		t.Fatalf("Update changed CreatedAt = %v, want unchanged %v", opt.CreatedAt, createdAt)
	}
	if opt.UpdatedAt.IsZero() {
		t.Fatal("Update left UpdatedAt at the zero value, want the row's real updated_at")
	}
	if !opt.UpdatedAt.After(updatedAtBefore) {
		t.Fatalf("Update's UpdatedAt = %v, want newer than pre-update %v", opt.UpdatedAt, updatedAtBefore)
	}
}

// TestLoadTasteMappingsIncludesHiddenOptions pins spec criterion 12
// ("a hidden option still scores in personalization until the guest's next
// save"): a cuisine tile and a budget tier are hidden through the REAL write
// path (repo.Update setting IsActive=false, the same call
// usecase/foodieoptions.UseCase.Update makes — not a raw SQL UPDATE), and
// LoadTasteMappings — the one query tastematch.Loader relies on — must still
// return both, alongside an untouched active one. LoadTasteMappings had no
// repository test at all before this.
func TestLoadTasteMappingsIncludesHiddenOptions(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options", "cuisines")
	ctx := context.Background()
	repo := New(pool)
	cuisines := cuisinerepo.New(pool)

	italian := &domain.Cuisine{ID: uuid.New(), Code: "italian", Name: "Итальянская", IsActive: true}
	kazakh := &domain.Cuisine{ID: uuid.New(), Code: "kazakh", Name: "Казахская", IsActive: true}
	if err := cuisines.Create(ctx, italian); err != nil {
		t.Fatalf("seed cuisine italian: %v", err)
	}
	if err := cuisines.Create(ctx, kazakh); err != nil {
		t.Fatalf("seed cuisine kazakh: %v", err)
	}

	// Active tile, stays active — the control.
	activeTile := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindCuisine, Code: "italian", Name: "Италия", IsActive: true}
	if err := repo.Create(ctx, activeTile); err != nil {
		t.Fatalf("create active tile: %v", err)
	}
	if err := repo.SetCuisineLinks(ctx, activeTile.ID, []uuid.UUID{italian.ID}); err != nil {
		t.Fatalf("link active tile: %v", err)
	}

	// Hidden tile: created active (a guest could have picked it before an
	// admin hid it), then hidden through repo.Update — the same call the
	// usecase's Update/SetActive make.
	hiddenTile := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindCuisine, Code: "kazakh", Name: "Казахстан", IsActive: true}
	if err := repo.Create(ctx, hiddenTile); err != nil {
		t.Fatalf("create hidden tile: %v", err)
	}
	if err := repo.SetCuisineLinks(ctx, hiddenTile.ID, []uuid.UUID{kazakh.ID}); err != nil {
		t.Fatalf("link hidden tile: %v", err)
	}
	hiddenTile.IsActive = false
	if err := repo.Update(ctx, hiddenTile); err != nil {
		t.Fatalf("hide tile: %v", err)
	}

	// Hidden budget tier, same story: a guest may still hold "ultra" in
	// users.foodie_budget_tier after the admin hides it.
	hiddenBudget := &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindBudget, Code: "ultra", Name: "Ultra", PriceCategory: priceCat(domain.PriceHigh), IsActive: true}
	if err := repo.Create(ctx, hiddenBudget); err != nil {
		t.Fatalf("create hidden budget: %v", err)
	}
	hiddenBudget.IsActive = false
	if err := repo.Update(ctx, hiddenBudget); err != nil {
		t.Fatalf("hide budget: %v", err)
	}
	if hiddenTile.IsActive || hiddenBudget.IsActive {
		t.Fatal("test setup bug: hidden fixtures were not actually hidden")
	}

	cuisineTiles, budgetTiers, err := repo.LoadTasteMappings(ctx)
	if err != nil {
		t.Fatalf("LoadTasteMappings: %v", err)
	}

	if got := cuisineTiles["italian"]; len(got) != 1 || got[0] != "italian" {
		t.Errorf("cuisineTiles[italian] (active) = %v, want [italian]", got)
	}
	if got := cuisineTiles["kazakh"]; len(got) != 1 || got[0] != "kazakh" {
		t.Errorf("cuisineTiles[kazakh] (HIDDEN) = %v, want [kazakh] — a hidden tile must keep resolving", got)
	}
	if got, ok := budgetTiers["ultra"]; !ok || got != domain.PriceHigh {
		t.Errorf("budgetTiers[ultra] (HIDDEN) = %v, ok=%v, want PriceHigh — a hidden budget tier must keep resolving", got, ok)
	}
}

func TestUpdateNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	err := repo.Update(ctx, &domain.FoodieOption{ID: uuid.New(), Kind: domain.FoodieOptionKindDiet, Code: "ghost", Name: "Ghost"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update missing id = %v, want ErrNotFound", err)
	}
}

func TestCountActiveAndAllExistingCodes(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "foodie_options")
	ctx := context.Background()
	repo := New(pool)

	mustCreate := func(kind domain.FoodieOptionKind, code string, active bool) {
		t.Helper()
		if err := repo.Create(ctx, &domain.FoodieOption{
			ID: uuid.New(), Kind: kind, Code: code, Name: "Name " + code, IsActive: active,
		}); err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
	}
	mustCreate(domain.FoodieOptionKindDiet, "no_diet", true)
	mustCreate(domain.FoodieOptionKindDiet, "halal", true)
	mustCreate(domain.FoodieOptionKindDiet, "spicy_diet", false)
	mustCreate(domain.FoodieOptionKindCuisine, "kazakh", true)

	n, err := repo.CountActive(ctx, domain.FoodieOptionKindDiet)
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountActive(diet) = %d, want 2", n)
	}
	if n, err := repo.CountActive(ctx, domain.FoodieOptionKindBudget); err != nil || n != 0 {
		t.Fatalf("CountActive(budget) = %d, %v, want 0, nil", n, err)
	}

	// AllExistingCodes includes the HIDDEN one too (criterion 9: an old
	// client's code must still be accepted after it is hidden), covers every
	// kind in the SAME read, and does not mix kinds up.
	byKind, err := repo.AllExistingCodes(ctx)
	if err != nil {
		t.Fatalf("AllExistingCodes: %v", err)
	}
	diets := byKind[domain.FoodieOptionKindDiet]
	for _, want := range []string{"no_diet", "halal", "spicy_diet"} {
		if _, ok := diets[want]; !ok {
			t.Errorf("AllExistingCodes[diet] missing %q: %v", want, diets)
		}
	}
	if _, ok := diets["unknown"]; ok {
		t.Error("AllExistingCodes[diet] contains an id that was never created")
	}
	if _, ok := byKind[domain.FoodieOptionKindCuisine]["kazakh"]; !ok {
		t.Errorf("AllExistingCodes[cuisine] missing kazakh: %v", byKind[domain.FoodieOptionKindCuisine])
	}
	if _, ok := byKind[domain.FoodieOptionKindCuisine]["halal"]; ok {
		t.Error("AllExistingCodes[cuisine] leaked a diet code")
	}
}
