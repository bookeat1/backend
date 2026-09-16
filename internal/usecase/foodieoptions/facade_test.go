package foodieoptions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes -------------------------------------------------------------

type memRepo struct {
	items map[uuid.UUID]domain.FoodieOption
	links map[uuid.UUID][]uuid.UUID
}

func newMemRepo() *memRepo {
	return &memRepo{items: map[uuid.UUID]domain.FoodieOption{}, links: map[uuid.UUID][]uuid.UUID{}}
}

func (m *memRepo) add(o domain.FoodieOption) domain.FoodieOption {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	m.items[o.ID] = o
	return o
}

func (m *memRepo) List(_ context.Context, f domain.FoodieOptionFilter) ([]domain.FoodieOption, error) {
	out := make([]domain.FoodieOption, 0, len(m.items))
	for _, o := range m.items {
		if f.Kind != nil && o.Kind != *f.Kind {
			continue
		}
		if o.IsActive || f.IncludeInactive {
			out = append(out, o)
		}
	}
	return out, nil
}

func (m *memRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.FoodieOption, error) {
	o, ok := m.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &o, nil
}

func (m *memRepo) Create(_ context.Context, o *domain.FoodieOption) error {
	for _, existing := range m.items {
		if existing.Kind == o.Kind && existing.Code == o.Code {
			return domain.ErrAlreadyExists
		}
	}
	m.items[o.ID] = *o
	return nil
}

func (m *memRepo) Update(_ context.Context, o *domain.FoodieOption) error {
	if _, ok := m.items[o.ID]; !ok {
		return domain.ErrNotFound
	}
	m.items[o.ID] = *o
	return nil
}

func (m *memRepo) SetCuisineLinks(_ context.Context, optionID uuid.UUID, cuisineIDs []uuid.UUID) error {
	m.links[optionID] = append([]uuid.UUID(nil), cuisineIDs...)
	return nil
}

func (m *memRepo) CountActive(_ context.Context, kind domain.FoodieOptionKind) (int, error) {
	n := 0
	for _, o := range m.items {
		if o.Kind == kind && o.IsActive {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) AllExistingCodes(_ context.Context) (map[domain.FoodieOptionKind]map[string]struct{}, error) {
	out := map[domain.FoodieOptionKind]map[string]struct{}{}
	for _, o := range m.items {
		if out[o.Kind] == nil {
			out[o.Kind] = map[string]struct{}{}
		}
		out[o.Kind][o.Code] = struct{}{}
	}
	return out, nil
}

type memCuisines struct{ items map[uuid.UUID]domain.Cuisine }

func newMemCuisines() *memCuisines { return &memCuisines{items: map[uuid.UUID]domain.Cuisine{}} }

func (m *memCuisines) add(c domain.Cuisine) domain.Cuisine {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	m.items[c.ID] = c
	return c
}

func (m *memCuisines) ResolveIDs(_ context.Context, ids []uuid.UUID) ([]domain.Cuisine, error) {
	out := make([]domain.Cuisine, 0, len(ids))
	for _, id := range ids {
		c, ok := m.items[id]
		if !ok {
			return nil, domain.ErrValidation
		}
		out = append(out, c)
	}
	return out, nil
}

type passthroughTx struct{}

func (passthroughTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (passthroughTx) Detach(ctx context.Context) context.Context { return ctx }

func newUC(repo *memRepo, cuisines *memCuisines) UseCase {
	return NewUseCase(repo, cuisines, passthroughTx{})
}

// --- tests ---------------------------------------------------------------

func TestOnlyPlatformManagesTheDictionary(t *testing.T) {
	repo := newMemRepo()
	existing := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", IsActive: true})
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: "other", Name: "Other", IsActive: true})
	u := newUC(repo, newMemCuisines())
	ctx := context.Background()

	kind := domain.FoodieOptionKindDiet
	code, name := "kosher", "Кошерное"
	in := SaveInput{Kind: &kind, Code: &code, Name: &name}

	for _, role := range []domain.Role{domain.RoleRestaurant, domain.RoleUser, ""} {
		venue := Actor{UserID: uuid.New(), Role: role}
		if _, err := u.Create(ctx, venue, in); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("Create as %q = %v, want ErrForbidden", role, err)
		}
		if _, err := u.Update(ctx, venue, existing.ID, in); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("Update as %q = %v, want ErrForbidden", role, err)
		}
		if _, err := u.SetActive(ctx, venue, existing.ID, false); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("SetActive as %q = %v, want ErrForbidden", role, err)
		}
	}

	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}
	created, err := u.Create(ctx, admin, in)
	if err != nil {
		t.Fatalf("Create as superadmin: %v", err)
	}
	if created.Code != "kosher" || !created.IsActive {
		t.Errorf("created = %+v, want an active entry with code kosher", created)
	}
}

func TestHiddenEntriesAreOnlyVisibleToThePlatform(t *testing.T) {
	repo := newMemRepo()
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindAllergy, Code: "nuts", Name: "Орехи", IsActive: true})
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindAllergy, Code: "soy", Name: "Соя", IsActive: false})
	u := newUC(repo, newMemCuisines())
	ctx := context.Background()

	guest, err := u.List(ctx, Actor{}, false)
	if err != nil || len(guest) != 1 {
		t.Fatalf("guest list = %v (%d entries), want only the active one", err, len(guest))
	}
	sneaky, err := u.List(ctx, Actor{Role: domain.RoleUser}, true)
	if err != nil || len(sneaky) != 1 {
		t.Fatalf("non-admin includeInactive list = %v (%d entries), want 1", err, len(sneaky))
	}
	admin, err := u.List(ctx, Actor{Role: domain.RoleAdmin}, true)
	if err != nil || len(admin) != 2 {
		t.Fatalf("admin list = %v (%d entries), want both", err, len(admin))
	}
}

// TestNoDietCannotBeHidden pins the one code that must never be hideable
// (spec 3.6): it is the sole way a guest expresses "no diet", and the
// exclusivity rule (validateFoodieDiets in usecase/users) hangs off it.
func TestNoDietCannotBeHidden(t *testing.T) {
	repo := newMemRepo()
	noDiet := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: domain.FoodieDietExclusiveID, Name: "Без диеты", IsActive: true})
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", IsActive: true})
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	_, err := u.SetActive(context.Background(), admin, noDiet.ID, false)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("hide no_diet = %v, want ErrValidation", err)
	}
}

// TestLastActiveOfKindCannotBeHidden pins criterion 8/spec 3.6: the wizard's
// cuisine/diet/allergy/budget steps each need at least one option to remain
// choosable (budget itself is optional, but the same rule applies uniformly
// per the spec's own §5 state machine).
func TestLastActiveOfKindCannotBeHidden(t *testing.T) {
	repo := newMemRepo()
	only := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindAllergy, Code: "nuts", Name: "Орехи", IsActive: true})
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	if _, err := u.SetActive(context.Background(), admin, only.ID, false); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("hide last active = %v, want ErrValidation", err)
	}

	// A second active option makes hiding the first one fine again.
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindAllergy, Code: "soy", Name: "Соя", IsActive: true})
	got, err := u.SetActive(context.Background(), admin, only.ID, false)
	if err != nil {
		t.Fatalf("hide with a sibling active: %v", err)
	}
	if got.IsActive {
		t.Error("SetActive(false) left IsActive true")
	}
}

// TestRestoreIsAlwaysAllowed: un-hiding never runs the hide guards, even when
// it is currently the only option of its kind (it obviously already is, or it
// could not have been hidden in the first place, but the guard must never
// fire on a true->true or false->true transition).
func TestRestoreIsAlwaysAllowed(t *testing.T) {
	repo := newMemRepo()
	hidden := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindBudget, Code: "mid", Name: "Средний", IsActive: false})
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	got, err := u.SetActive(context.Background(), admin, hidden.ID, true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !got.IsActive {
		t.Error("SetActive(true) left IsActive false")
	}
}

// TestKindAndCodeAreImmutable pins criterion 7.
func TestKindAndCodeAreImmutable(t *testing.T) {
	repo := newMemRepo()
	existing := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindCuisine, Code: "asian", Name: "Азиатская", IsActive: true})
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	otherCode := "european"
	if _, err := u.Update(context.Background(), admin, existing.ID, SaveInput{Code: &otherCode}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("change code = %v, want ErrValidation", err)
	}
	otherKind := domain.FoodieOptionKindDiet
	if _, err := u.Update(context.Background(), admin, existing.ID, SaveInput{Kind: &otherKind}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("change kind = %v, want ErrValidation", err)
	}
	// Resending the SAME code/kind alongside another field is fine.
	sameCode := "asian"
	newName := "Паназиатская"
	got, err := u.Update(context.Background(), admin, existing.ID, SaveInput{Code: &sameCode, Name: &newName})
	if err != nil {
		t.Fatalf("resend same code: %v", err)
	}
	if got.Name != newName {
		t.Errorf("name = %q, want %q", got.Name, newName)
	}
}

// TestCuisineLinksOnlyForCuisineKindAndMustBeActive pins criterion 6/3.11.
func TestCuisineLinksOnlyForCuisineKindAndMustBeActive(t *testing.T) {
	repo := newMemRepo()
	diet := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", IsActive: true})
	cuisineOpt := repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindCuisine, Code: "korean", Name: "Корейская", IsActive: true})
	cuisines := newMemCuisines()
	activeCuisine := cuisines.add(domain.Cuisine{Code: "korean", Name: "Корейская", IsActive: true})
	hiddenCuisine := cuisines.add(domain.Cuisine{Code: "old", Name: "Старая", IsActive: false})
	u := newUC(repo, cuisines)
	admin := Actor{Role: domain.RoleAdmin}

	// cuisine_ids on a non-cuisine kind is refused.
	ids := []uuid.UUID{activeCuisine.ID}
	if _, err := u.Update(context.Background(), admin, diet.ID, SaveInput{CuisineIDs: &ids}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("cuisine_ids on diet = %v, want ErrValidation", err)
	}

	// A hidden dictionary cuisine must not be newly linked.
	hiddenIDs := []uuid.UUID{hiddenCuisine.ID}
	if _, err := u.Update(context.Background(), admin, cuisineOpt.ID, SaveInput{CuisineIDs: &hiddenIDs}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("link a hidden cuisine = %v, want ErrValidation", err)
	}

	// An active cuisine links fine and AffectsMatching flips true.
	got, err := u.Update(context.Background(), admin, cuisineOpt.ID, SaveInput{CuisineIDs: &ids})
	if err != nil {
		t.Fatalf("link active cuisine: %v", err)
	}
	if len(got.Cuisines) != 1 || got.Cuisines[0].Code != "korean" {
		t.Fatalf("Cuisines = %v, want [korean]", got.Cuisines)
	}
	if !got.AffectsMatching() {
		t.Error("AffectsMatching() = false with a cuisine link, want true")
	}
}

// TestPriceCategoryOnlyForBudget pins criterion 6.
func TestPriceCategoryOnlyForBudget(t *testing.T) {
	repo := newMemRepo()
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	cuisineKind := domain.FoodieOptionKindCuisine
	code, name := "kazakh", "Казахская"
	pc := string(domain.PriceHigh)
	if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &cuisineKind, Code: &code, Name: &name, PriceCategory: &pc}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("price_category on cuisine = %v, want ErrValidation", err)
	}

	budgetKind := domain.FoodieOptionKindBudget
	bCode, bName := "ultra", "Ультра"
	bad := "₸₸₸₸"
	if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &budgetKind, Code: &bCode, Name: &bName, PriceCategory: &bad}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("invalid price_category = %v, want ErrValidation", err)
	}

	good := string(domain.PriceHigh)
	got, err := u.Create(context.Background(), admin, SaveInput{Kind: &budgetKind, Code: &bCode, Name: &bName, PriceCategory: &good})
	if err != nil {
		t.Fatalf("create budget with valid price_category: %v", err)
	}
	if got.PriceCategory == nil || *got.PriceCategory != domain.PriceHigh {
		t.Fatalf("PriceCategory = %v, want %q", got.PriceCategory, domain.PriceHigh)
	}

	// Clearing it back out (spec 3.10: a tier can have no ₸-equivalent).
	empty := ""
	got, err = u.Update(context.Background(), admin, got.ID, SaveInput{PriceCategory: &empty})
	if err != nil {
		t.Fatalf("clear price_category: %v", err)
	}
	if got.PriceCategory != nil {
		t.Fatalf("PriceCategory after clear = %v, want nil", got.PriceCategory)
	}
}

// TestCodeValidation mirrors usecase/cuisines.TestCodeValidation, plus the
// budget-only shorter length cap.
func TestCodeValidation(t *testing.T) {
	repo := newMemRepo()
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}
	kind := domain.FoodieOptionKindCuisine
	name := "Кухня"

	for _, bad := range []string{"", "Кухня", "pan asian", "pan-asian", "PanAsian!"} {
		code := bad
		if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &kind, Code: &code, Name: &name}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create with code %q = %v, want ErrValidation", bad, err)
		}
	}
	code := "  PAN_ASIAN "
	got, err := u.Create(context.Background(), admin, SaveInput{Kind: &kind, Code: &code, Name: &name})
	if err != nil {
		t.Fatalf("Create with a paddable code: %v", err)
	}
	if got.Code != "pan_asian" {
		t.Errorf("code = %q, want %q", got.Code, "pan_asian")
	}

	// Budget's narrower length cap: 17 chars is one over maxBudgetCodeLen.
	budgetKind := domain.FoodieOptionKindBudget
	longCode := "a_very_long_code_"
	if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &budgetKind, Code: &longCode, Name: &name}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Create budget with a 17-char code = %v, want ErrValidation", err)
	}
}

// TestNameLengthCapped pins spec 6.8.
func TestNameLengthCapped(t *testing.T) {
	repo := newMemRepo()
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}
	kind := domain.FoodieOptionKindCuisine
	code := "long"
	name := "1234567890123456789012345678901234567890X" // 41 runes
	if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &kind, Code: &code, Name: &name}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("41-rune name = %v, want ErrValidation", err)
	}
}

// TestDuplicateCodeOrName exercises the usecase surfacing repo.Create's
// ErrAlreadyExists untouched (the repository's unique indexes are the real
// guard, criterion 6/3.8 — the usecase must not swallow or remap it).
func TestDuplicateCodeOrName(t *testing.T) {
	repo := newMemRepo()
	repo.add(domain.FoodieOption{Kind: domain.FoodieOptionKindDiet, Code: "kosher", Name: "Кошерное", IsActive: true})
	u := newUC(repo, newMemCuisines())
	admin := Actor{Role: domain.RoleAdmin}

	kind := domain.FoodieOptionKindDiet
	code := "kosher"
	name := "Другое"
	if _, err := u.Create(context.Background(), admin, SaveInput{Kind: &kind, Code: &code, Name: &name}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("dup code = %v, want ErrAlreadyExists", err)
	}
}
