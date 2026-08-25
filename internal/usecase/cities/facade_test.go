package cities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ------------------------------------------------------------------

type fakeRepo struct {
	items    map[uuid.UUID]*domain.CityEntry
	aliases  map[string]uuid.UUID
	creates  int
	updates  int
	reorders [][]uuid.UUID
	getErr   error
}

func newRepo(seed ...*domain.CityEntry) *fakeRepo {
	r := &fakeRepo{items: map[uuid.UUID]*domain.CityEntry{}, aliases: map[string]uuid.UUID{}}
	for _, c := range seed {
		cp := *c
		r.items[c.ID] = &cp
		r.aliases[domain.NormalizeCityKey(c.Name)] = c.ID
		r.aliases[domain.NormalizeCityKey(c.Code)] = c.ID
	}
	return r
}

func (r *fakeRepo) List(_ context.Context, f domain.CityFilter) ([]domain.CityEntry, error) {
	out := make([]domain.CityEntry, 0, len(r.items))
	for _, c := range r.items {
		if !f.IncludeInactive && !c.IsActive {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (r *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.CityEntry, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	c, ok := r.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) Create(_ context.Context, c *domain.CityEntry) error {
	r.creates++
	cp := *c
	r.items[c.ID] = &cp
	r.aliases[domain.NormalizeCityKey(c.Name)] = c.ID
	return nil
}

func (r *fakeRepo) Update(_ context.Context, c *domain.CityEntry) error {
	r.updates++
	if _, ok := r.items[c.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *c
	r.items[c.ID] = &cp
	r.aliases[domain.NormalizeCityKey(c.Name)] = c.ID
	return nil
}

func (r *fakeRepo) Reorder(_ context.Context, ids []uuid.UUID) error {
	r.reorders = append(r.reorders, ids)
	return nil
}

func (r *fakeRepo) ResolveAlias(_ context.Context, raw string) (*domain.CityEntry, error) {
	id, ok := r.aliases[domain.NormalizeCityKey(raw)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *r.items[id]
	return &cp, nil
}

func (r *fakeRepo) AddAlias(_ context.Context, id uuid.UUID, alias string) error {
	key := domain.NormalizeCityKey(alias)
	if owner, ok := r.aliases[key]; ok && owner != id {
		return domain.ErrAlreadyExists
	}
	r.aliases[key] = id
	return nil
}

var _ domain.CityRepository = (*fakeRepo)(nil)

// fakeVenues records the derived-string rewrite and, crucially, whether it
// happened INSIDE the transaction the usecase opened.
type fakeVenues struct {
	calls    []uuid.UUID
	names    []string
	inTx     []bool
	insideTx func() bool
	err      error
}

func (f *fakeVenues) RenameCityString(_ context.Context, id uuid.UUID, name string) (int64, error) {
	f.calls = append(f.calls, id)
	f.names = append(f.names, name)
	f.inTx = append(f.inTx, f.insideTx())
	return 2, f.err
}

// fakeTx is a TxManager that only records whether the callback ran inside it.
type fakeTx struct {
	depth int
	runs  int
}

// Detach exists only to satisfy domain.TxManager; nothing in this package
// escapes its transaction.
func (t *fakeTx) Detach(ctx context.Context) context.Context { return ctx }

func (t *fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.runs++
	t.depth++
	defer func() { t.depth-- }()
	return fn(ctx)
}

func newDeps(seed ...*domain.CityEntry) (*fakeRepo, *fakeVenues, *fakeTx, UseCase) {
	repo := newRepo(seed...)
	tx := &fakeTx{}
	venues := &fakeVenues{insideTx: func() bool { return tx.depth > 0 }}
	return repo, venues, tx, NewUseCase(repo, venues, tx)
}

func almaty() *domain.CityEntry {
	return &domain.CityEntry{ID: uuid.New(), Code: "almaty", Name: "Алматы", DisplayOrder: 20, IsActive: true}
}

var admin = Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

// --- tests ------------------------------------------------------------------

// TestOnlyThePlatformMayManageTheDictionary is the usecase half of the gate the
// router also enforces. Two layers on purpose: the router gate is a mount-point
// decision that a later refactor can widen by accident, and the dictionary is
// exactly the thing a venue must not be able to extend for itself.
func TestOnlyThePlatformMayManageTheDictionary(t *testing.T) {
	city := almaty()
	name := "Что угодно"

	for _, role := range []domain.Role{domain.RoleRestaurant, domain.RoleUser, ""} {
		repo, venues, _, u := newDeps(city)
		actor := Actor{UserID: uuid.New(), Role: role}
		ctx := context.Background()

		mutations := map[string]error{}
		_, err := u.Create(ctx, actor, SaveInput{Code: strptr("shymkent"), Name: &name})
		mutations["create"] = err
		_, err = u.Update(ctx, actor, city.ID, SaveInput{Name: &name})
		mutations["update"] = err
		_, err = u.SetActive(ctx, actor, city.ID, false)
		mutations["hide"] = err
		_, err = u.Reorder(ctx, actor, []uuid.UUID{city.ID})
		mutations["reorder"] = err
		_, err = u.AddAlias(ctx, actor, city.ID, "алма-ата")
		mutations["alias"] = err

		for op, err := range mutations {
			if !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("%s as role %q = %v, want ErrForbidden", op, role, err)
			}
		}
		if repo.creates != 0 || repo.updates != 0 || len(repo.reorders) != 0 || len(venues.calls) != 0 {
			t.Errorf("role %q reached the repository: creates=%d updates=%d reorders=%d venue-writes=%d",
				role, repo.creates, repo.updates, len(repo.reorders), len(venues.calls))
		}
	}

	// The reads stay open — the city chips are a public screen.
	repo, _, _, u := newDeps(city)
	if _, err := u.List(context.Background(), Actor{Role: domain.RoleUser}, false); err != nil {
		t.Fatalf("public List = %v, want no error", err)
	}
	_ = repo
}

// TestGuestCannotSeeHiddenCities: includeInactive is honoured only for the
// platform. A guest asking for it gets the active list rather than a 403,
// because this route is called anonymously and a refusal would break the
// screen, but a hidden city must not leak either way.
func TestGuestCannotSeeHiddenCities(t *testing.T) {
	hidden := &domain.CityEntry{ID: uuid.New(), Code: "shymkent", Name: "Шымкент", IsActive: false}
	_, _, _, u := newDeps(almaty(), hidden)

	got, err := u.List(context.Background(), Actor{Role: domain.RoleUser}, true)
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	for _, c := range got {
		if c.Code == "shymkent" {
			t.Fatal("a guest asking includeInactive=true received a hidden city")
		}
	}

	got, err = u.List(context.Background(), admin, true)
	if err != nil {
		t.Fatalf("admin List = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("admin sees %d cities, want 2 including the hidden one", len(got))
	}
}

// TestRenameRewritesVenueStringsInTheSameTransaction is the data-integrity
// heart of this feature. `restaurants.city` is the derived rendering the store
// build reads; if the dictionary row were renamed without it, the catalog and
// the venues would disagree about where a venue is, and the old client's
// ?city= filter would match neither spelling.
func TestRenameRewritesVenueStringsInTheSameTransaction(t *testing.T) {
	city := almaty()
	repo, venues, tx, u := newDeps(city)

	newName := "Алматы Сити"
	out, err := u.Update(context.Background(), admin, city.ID, SaveInput{Name: &newName})
	if err != nil {
		t.Fatalf("Update = %v", err)
	}
	if out.Name != newName {
		t.Fatalf("name = %q, want %q", out.Name, newName)
	}
	if tx.runs != 1 {
		t.Fatalf("transactions opened = %d, want exactly 1", tx.runs)
	}
	if len(venues.calls) != 1 || venues.calls[0] != city.ID {
		t.Fatalf("venue rewrite calls = %v, want one for %s", venues.calls, city.ID)
	}
	if venues.names[0] != newName {
		t.Errorf("venues rewritten to %q, want %q", venues.names[0], newName)
	}
	if !venues.inTx[0] {
		t.Error("the venue rewrite ran OUTSIDE the transaction: a failure would leave the two halves disagreeing")
	}
	if repo.updates != 1 {
		t.Errorf("dictionary updates = %d, want 1", repo.updates)
	}
	// The new spelling must be resolvable straight away, or the database
	// trigger would null out city_id on the very rows just rewritten.
	if _, err := repo.ResolveAlias(context.Background(), newName); err != nil {
		t.Errorf("the new name did not become an alias: %v", err)
	}
	// ...and so must the old one: a phone still filtering by it keeps working.
	if _, err := repo.ResolveAlias(context.Background(), "Алматы"); err != nil {
		t.Errorf("the previous name stopped resolving: %v", err)
	}
}

// TestNonRenameUpdateLeavesVenuesAlone: reordering or hiding a city must not
// touch a single venue row. Rewriting 43 venues because someone dragged a chip
// is both pointless and a way to lose an edit made in the panel a second
// earlier.
func TestNonRenameUpdateLeavesVenuesAlone(t *testing.T) {
	city := almaty()
	_, venues, _, u := newDeps(city)

	order := 5
	if _, err := u.Update(context.Background(), admin, city.ID, SaveInput{DisplayOrder: &order}); err != nil {
		t.Fatalf("Update = %v", err)
	}
	if _, err := u.SetActive(context.Background(), admin, city.ID, false); err != nil {
		t.Fatalf("SetActive = %v", err)
	}
	if len(venues.calls) != 0 {
		t.Fatalf("venues were rewritten %d times by a non-rename update", len(venues.calls))
	}
}

// TestHideKeepsTheEntry: hiding is the ONLY removal this dictionary has, and it
// must leave the row (and therefore every venue's foreign key) intact.
func TestHideKeepsTheEntry(t *testing.T) {
	city := almaty()
	repo, _, _, u := newDeps(city)

	out, err := u.SetActive(context.Background(), admin, city.ID, false)
	if err != nil {
		t.Fatalf("SetActive = %v", err)
	}
	if out.IsActive {
		t.Fatal("entry still active after hiding")
	}
	stored, err := repo.GetByID(context.Background(), city.ID)
	if err != nil {
		t.Fatalf("the entry is gone after hiding: %v", err)
	}
	if stored.IsActive {
		t.Error("stored entry still active")
	}
	if stored.Name != city.Name || stored.Code != city.Code {
		t.Error("hiding changed the identity of the entry")
	}

	// And it comes back.
	back, err := u.SetActive(context.Background(), admin, city.ID, true)
	if err != nil || !back.IsActive {
		t.Fatalf("restore = %v, active=%v", err, back != nil && back.IsActive)
	}
}

// TestResolveTreatsAnUnknownCityAsAnAnswer: an unrecognised spelling is a
// normal outcome (that is precisely the case this feature has to survive), not
// an error that would turn a catalog request into a 500.
func TestResolveTreatsAnUnknownCityAsAnAnswer(t *testing.T) {
	city := almaty()
	_, _, _, u := newDeps(city)
	ctx := context.Background()

	for _, raw := range []string{"Алматы", "  алматы ", "almaty", "ALMATY"} {
		got, err := u.Resolve(ctx, raw)
		if err != nil {
			t.Fatalf("Resolve(%q) = %v", raw, err)
		}
		if got == nil || got.ID != city.ID {
			t.Errorf("Resolve(%q) did not find the city", raw)
		}
	}

	for _, raw := range []string{"Караганда", "", "   "} {
		got, err := u.Resolve(ctx, raw)
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want a nil answer without an error", raw, err)
		}
		if got != nil {
			t.Errorf("Resolve(%q) invented a city %q", raw, got.Code)
		}
	}
}

// TestCreateValidatesTheMachineKey: code travels in query strings and keys the
// client's local assets, so anything but lowercase latin/digits/underscore has
// to be refused at the door — not stored and discovered later.
func TestCreateValidatesTheMachineKey(t *testing.T) {
	repo, _, _, u := newDeps()
	name := "Шымкент"

	for _, bad := range []string{"", "Шымкент", "shym kent", "shym-kent", "SHYMKENT!"} {
		code := bad
		if _, err := u.Create(context.Background(), admin, SaveInput{Code: &code, Name: &name}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create with code %q = %v, want ErrValidation", bad, err)
		}
	}
	if repo.creates != 0 {
		t.Fatalf("%d invalid entries reached the repository", repo.creates)
	}

	code := "shymkent"
	out, err := u.Create(context.Background(), admin, SaveInput{Code: &code, Name: &name})
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if out.Code != code || !out.IsActive {
		t.Errorf("created entry = %+v, want an active %q", out, code)
	}
}

func strptr(s string) *string { return &s }
