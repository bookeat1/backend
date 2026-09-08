package cuisines

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ------------------------------------------------------------------

type memRepo struct {
	items map[uuid.UUID]domain.Cuisine
	links map[uuid.UUID][]uuid.UUID
}

func newMemRepo() *memRepo {
	return &memRepo{items: map[uuid.UUID]domain.Cuisine{}, links: map[uuid.UUID][]uuid.UUID{}}
}

func (m *memRepo) add(c domain.Cuisine) domain.Cuisine {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	m.items[c.ID] = c
	return c
}

func (m *memRepo) List(_ context.Context, f domain.CuisineFilter) ([]domain.Cuisine, error) {
	out := make([]domain.Cuisine, 0, len(m.items))
	for _, c := range m.items {
		if c.IsActive || f.IncludeInactive {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Cuisine, error) {
	c, ok := m.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &c, nil
}

func (m *memRepo) Create(_ context.Context, c *domain.Cuisine) error {
	for _, existing := range m.items {
		if existing.Code == c.Code {
			return domain.ErrAlreadyExists
		}
	}
	m.items[c.ID] = *c
	return nil
}

func (m *memRepo) Update(_ context.Context, c *domain.Cuisine) error {
	if _, ok := m.items[c.ID]; !ok {
		return domain.ErrNotFound
	}
	m.items[c.ID] = *c
	return nil
}

func (m *memRepo) ListByRestaurants(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.Cuisine, error) {
	out := map[uuid.UUID][]domain.Cuisine{}
	for _, rid := range ids {
		for _, cid := range m.links[rid] {
			out[rid] = append(out[rid], m.items[cid])
		}
	}
	return out, nil
}

func (m *memRepo) ResolveIDs(_ context.Context, ids []uuid.UUID) ([]domain.Cuisine, error) {
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

func (m *memRepo) SetForRestaurant(_ context.Context, rid uuid.UUID, ids []uuid.UUID) error {
	m.links[rid] = append([]uuid.UUID(nil), ids...)
	return nil
}

// memVenues records the derived cuisine_type writes so a test can assert what
// an old store build would see.
type memVenues struct {
	lastID   uuid.UUID
	lastText string
	lastI18n domain.I18n
	calls    int
}

func (m *memVenues) UpdateCuisineTypeString(_ context.Context, id uuid.UUID, text string, i18n domain.I18n) error {
	m.lastID, m.lastText, m.lastI18n, m.calls = id, text, i18n, m.calls+1
	return nil
}

type memPerms struct{ allow bool }

func (m *memPerms) HasPermission(context.Context, uuid.UUID, uuid.UUID, domain.Permission) (bool, error) {
	return m.allow, nil
}

// passthroughTx runs fn inline: these tests are about the decisions, not about
// transaction plumbing (which is exercised against a real database).
type passthroughTx struct{}

func (passthroughTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (passthroughTx) Detach(ctx context.Context) context.Context { return ctx }

func newUC(repo *memRepo, venues *memVenues, perms *memPerms) UseCase {
	return NewUseCase(repo, venues, perms, passthroughTx{})
}

// --- tests ------------------------------------------------------------------

// TestOnlyPlatformManagesTheDictionary is the rule the whole feature rests on:
// a venue picks from the list, it never adds to it. Allowing otherwise
// reproduces «Кафе, европейская» in a new table within a month (ADR-022).
func TestOnlyPlatformManagesTheDictionary(t *testing.T) {
	repo := newMemRepo()
	existing := repo.add(domain.Cuisine{Code: "european", Name: "Европейская", IsActive: true})
	u := newUC(repo, &memVenues{}, &memPerms{allow: true})
	ctx := context.Background()

	code, name := "authors", "Авторская"
	in := SaveInput{Code: &code, Name: &name}

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
	if created.Code != "authors" || !created.IsActive {
		t.Errorf("created = %+v, want an active entry with code authors", created)
	}
}

// TestHiddenEntriesAreOnlyVisibleToThePlatform: hiding a cuisine must remove it
// from what a guest can pick, while staying recoverable by whoever hid it.
func TestHiddenEntriesAreOnlyVisibleToThePlatform(t *testing.T) {
	repo := newMemRepo()
	repo.add(domain.Cuisine{Code: "european", Name: "Европейская", IsActive: true})
	repo.add(domain.Cuisine{Code: "coffee", Name: "Кофейня", IsActive: false})
	u := newUC(repo, &memVenues{}, &memPerms{allow: true})
	ctx := context.Background()

	guest, err := u.List(ctx, Actor{}, false)
	if err != nil || len(guest) != 1 {
		t.Fatalf("guest list = %v (%d entries), want only the active one", err, len(guest))
	}
	// A guest ASKING for hidden entries still gets only the active ones,
	// rather than a 403: the anonymous route has no caller to refuse.
	sneaky, err := u.List(ctx, Actor{Role: domain.RoleUser}, true)
	if err != nil || len(sneaky) != 1 {
		t.Fatalf("non-admin includeInactive list = %v (%d entries), want 1", err, len(sneaky))
	}
	admin, err := u.List(ctx, Actor{Role: domain.RoleAdmin}, true)
	if err != nil || len(admin) != 2 {
		t.Fatalf("admin list = %v (%d entries), want both", err, len(admin))
	}
}

// TestSetForRestaurantRewritesTheLegacyString is the backward-compatibility
// contract: the store build reads ONE string, so changing the set must produce
// the comma-joined names — with the translations carried across too.
func TestSetForRestaurantRewritesTheLegacyString(t *testing.T) {
	repo := newMemRepo()
	italian := repo.add(domain.Cuisine{
		Code: "italian", Name: "Итальянская", IsActive: true,
		NameI18n: domain.I18n{"en": "Italian"},
	})
	european := repo.add(domain.Cuisine{
		Code: "european", Name: "Европейская", IsActive: true,
		NameI18n: domain.I18n{"en": "European"},
	})
	venues := &memVenues{}
	u := newUC(repo, venues, &memPerms{allow: true})
	ctx := context.Background()
	venue := uuid.New()
	owner := Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}

	set, err := u.SetForRestaurant(ctx, owner, venue, []uuid.UUID{italian.ID, european.ID})
	if err != nil {
		t.Fatalf("SetForRestaurant: %v", err)
	}
	if len(set) != 2 || set[0].Code != "italian" {
		t.Fatalf("returned set = %+v, want the requested order", set)
	}
	if venues.lastText != "Итальянская, Европейская" {
		t.Errorf("derived cuisine_type = %q, want %q", venues.lastText, "Итальянская, Европейская")
	}
	if venues.lastI18n["en"] != "Italian, European" {
		t.Errorf("derived cuisine_type_i18n[en] = %q, want %q", venues.lastI18n["en"], "Italian, European")
	}
	if _, hasRU := venues.lastI18n["ru"]; hasRU {
		t.Error("ru must not appear in the i18n map: it is the base column, not a translation")
	}

	// Clearing the set clears the string too — the venue said it has none.
	if _, err := u.SetForRestaurant(ctx, owner, venue, nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if venues.lastText != "" {
		t.Errorf("derived cuisine_type after clearing = %q, want empty", venues.lastText)
	}
}

// TestSetForRestaurantRefusals collects the ways a venue may NOT edit its set.
func TestSetForRestaurantRefusals(t *testing.T) {
	repo := newMemRepo()
	active := repo.add(domain.Cuisine{Code: "european", Name: "Европейская", IsActive: true})
	hidden := repo.add(domain.Cuisine{Code: "coffee", Name: "Кофейня", IsActive: false})
	venues := &memVenues{}
	ctx := context.Background()
	venue := uuid.New()

	// Not staff of this venue.
	stranger := newUC(repo, venues, &memPerms{allow: false})
	_, err := stranger.SetForRestaurant(ctx, Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		venue, []uuid.UUID{active.ID})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("set as a stranger = %v, want ErrForbidden", err)
	}
	if venues.calls != 0 {
		t.Error("a refused call must not have written the derived string")
	}

	u := newUC(repo, venues, &memPerms{allow: true})
	owner := Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}

	// A hidden entry must not spread: hiding it has to actually mean something.
	if _, err := u.SetForRestaurant(ctx, owner, venue, []uuid.UUID{hidden.ID}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("assigning a hidden cuisine = %v, want ErrValidation", err)
	}
	// An unknown id fails the whole call rather than being dropped silently.
	if _, err := u.SetForRestaurant(ctx, owner, venue, []uuid.UUID{uuid.New()}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("assigning an unknown cuisine = %v, want ErrValidation", err)
	}
	// The cap.
	many := make([]uuid.UUID, 0, MaxCuisinesPerVenue+1)
	for i := 0; i <= MaxCuisinesPerVenue; i++ {
		many = append(many, repo.add(domain.Cuisine{Code: "c" + string(rune('a'+i)), Name: "C", IsActive: true}).ID)
	}
	if _, err := u.SetForRestaurant(ctx, owner, venue, many); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("assigning %d cuisines = %v, want ErrValidation", len(many), err)
	}
}

// TestCodeValidation keeps Code usable as a machine key: clients derive their
// bundled fallback image from it and it travels in query strings.
func TestCodeValidation(t *testing.T) {
	repo := newMemRepo()
	u := newUC(repo, &memVenues{}, &memPerms{allow: true})
	admin := Actor{Role: domain.RoleAdmin}
	name := "Кухня"

	for _, bad := range []string{"", "Кухня", "pan asian", "pan-asian", "PanAsian!"} {
		code := bad
		if _, err := u.Create(context.Background(), admin, SaveInput{Code: &code, Name: &name}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create with code %q = %v, want ErrValidation", bad, err)
		}
	}
	// Uppercase is normalized rather than rejected — it is unambiguous.
	code := "  PAN_ASIAN "
	got, err := u.Create(context.Background(), admin, SaveInput{Code: &code, Name: &name})
	if err != nil {
		t.Fatalf("Create with a paddable code: %v", err)
	}
	if got.Code != "pan_asian" {
		t.Errorf("code = %q, want %q", got.Code, "pan_asian")
	}
}
