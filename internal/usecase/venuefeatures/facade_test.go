package venuefeatures

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ------------------------------------------------------------------

type memRepo struct {
	items map[uuid.UUID]domain.VenueFeature
	links map[uuid.UUID][]uuid.UUID
}

func newMemRepo() *memRepo {
	return &memRepo{items: map[uuid.UUID]domain.VenueFeature{}, links: map[uuid.UUID][]uuid.UUID{}}
}

func (m *memRepo) add(f domain.VenueFeature) domain.VenueFeature {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	m.items[f.ID] = f
	return f
}

func (m *memRepo) List(_ context.Context, f domain.VenueFeatureFilter) ([]domain.VenueFeature, error) {
	out := make([]domain.VenueFeature, 0, len(m.items))
	for _, vf := range m.items {
		if vf.IsActive || f.IncludeInactive {
			out = append(out, vf)
		}
	}
	return out, nil
}

func (m *memRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.VenueFeature, error) {
	vf, ok := m.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &vf, nil
}

func (m *memRepo) Create(_ context.Context, vf *domain.VenueFeature) error {
	for _, existing := range m.items {
		if existing.Code == vf.Code {
			return domain.ErrAlreadyExists
		}
	}
	m.items[vf.ID] = *vf
	return nil
}

func (m *memRepo) Update(_ context.Context, vf *domain.VenueFeature) error {
	if _, ok := m.items[vf.ID]; !ok {
		return domain.ErrNotFound
	}
	m.items[vf.ID] = *vf
	return nil
}

func (m *memRepo) ListByRestaurants(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.VenueFeature, error) {
	out := map[uuid.UUID][]domain.VenueFeature{}
	for _, rid := range ids {
		for _, fid := range m.links[rid] {
			out[rid] = append(out[rid], m.items[fid])
		}
	}
	return out, nil
}

func (m *memRepo) ResolveIDs(_ context.Context, ids []uuid.UUID) ([]domain.VenueFeature, error) {
	out := make([]domain.VenueFeature, 0, len(ids))
	for _, id := range ids {
		vf, ok := m.items[id]
		if !ok {
			return nil, domain.ErrValidation
		}
		out = append(out, vf)
	}
	return out, nil
}

func (m *memRepo) SetForRestaurant(_ context.Context, rid uuid.UUID, ids []uuid.UUID) error {
	m.links[rid] = append([]uuid.UUID(nil), ids...)
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

func newUC(repo *memRepo, perms *memPerms) UseCase {
	return NewUseCase(repo, perms, passthroughTx{})
}

func admin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func venueStaff() Actor {
	return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}
}

// --- tests ------------------------------------------------------------------

// TestOnlyPlatformManagesTheDictionary is the rule the whole feature rests on:
// a venue picks from the list, it never adds to it. Allowing otherwise
// reproduces the free-text mess in a new table — «Восточная кухня» and
// «Коктобе» filed as "удобства" — within a month.
func TestOnlyPlatformManagesTheDictionary(t *testing.T) {
	repo := newMemRepo()
	existing := repo.add(domain.VenueFeature{Code: "wifi", Name: "Wi-Fi", IsActive: true})
	u := newUC(repo, &memPerms{allow: true})
	ctx := context.Background()

	code, name := "hookah", "Кальян"
	if _, err := u.Create(ctx, venueStaff(), SaveInput{Code: &code, Name: &name}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("venue staff Create = %v, want ErrForbidden", err)
	}
	if _, err := u.Update(ctx, venueStaff(), existing.ID, SaveInput{Name: &name}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("venue staff Update = %v, want ErrForbidden", err)
	}
	if _, err := u.SetActive(ctx, venueStaff(), existing.ID, false); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("venue staff SetActive = %v, want ErrForbidden", err)
	}
	if _, err := u.Create(ctx, admin(), SaveInput{Code: &code, Name: &name}); err != nil {
		t.Errorf("admin Create = %v, want success", err)
	}
}

// TestCodeIsAStableMachineKey: the code travels in a query string and the app
// keys its icon off it, so Cyrillic, spaces and punctuation must be refused
// rather than quietly stored and later 404-ing an asset.
func TestCodeIsAStableMachineKey(t *testing.T) {
	u := newUC(newMemRepo(), &memPerms{allow: true})
	ctx := context.Background()
	name := "Кальян"

	for _, bad := range []string{"Кальян", "hookah bar", "hookah-bar", "hookah!", ""} {
		code := bad
		if _, err := u.Create(ctx, admin(), SaveInput{Code: &code, Name: &name}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create(code=%q) = %v, want ErrValidation", bad, err)
		}
	}
	good := "  HOOKAH  "
	out, err := u.Create(ctx, admin(), SaveInput{Code: &good, Name: &name})
	if err != nil {
		t.Fatalf("Create(code=%q) = %v, want success", good, err)
	}
	if out.Code != "hookah" {
		t.Errorf("code stored as %q, want it trimmed and lower-cased", out.Code)
	}
}

// TestVenueMustOwnTheVenueItEdits: the transport gate is not trusted alone.
func TestVenueMustOwnTheVenueItEdits(t *testing.T) {
	repo := newMemRepo()
	wifi := repo.add(domain.VenueFeature{Code: "wifi", Name: "Wi-Fi", IsActive: true})
	ctx := context.Background()
	venue := uuid.New()

	denied := newUC(repo, &memPerms{allow: false})
	if _, err := denied.SetForRestaurant(ctx, venueStaff(), venue, []uuid.UUID{wifi.ID}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("SetForRestaurant without the permission = %v, want ErrForbidden", err)
	}
	allowed := newUC(repo, &memPerms{allow: true})
	if _, err := allowed.SetForRestaurant(ctx, venueStaff(), venue, []uuid.UUID{wifi.ID}); err != nil {
		t.Errorf("SetForRestaurant with the permission = %v, want success", err)
	}
}

// TestHiddenFeatureCannotBeNewlyAssigned: hiding has to actually stop a feature
// spreading, or "скрыть" means nothing.
func TestHiddenFeatureCannotBeNewlyAssigned(t *testing.T) {
	repo := newMemRepo()
	gone := repo.add(domain.VenueFeature{Code: "wine_list", Name: "Винная карта", IsActive: false})
	u := newUC(repo, &memPerms{allow: true})

	_, err := u.SetForRestaurant(context.Background(), venueStaff(), uuid.New(), []uuid.UUID{gone.ID})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("assigning a hidden feature = %v, want ErrValidation", err)
	}
}

// TestOppositeFeaturesMayCoexist pins a DELIBERATE non-rule (owner, 2026-08-25).
//
// «Детские стульчики» and «Без детей» contradict each other in meaning but are
// not mutually exclusive technically, and no validation may pretend otherwise:
// a venue ticks what it likes, and an absurd combination is something a human
// spots while filling the data — not something a 422 should teach them at the
// worst possible moment. This test exists so nobody "fixes" that later.
func TestOppositeFeaturesMayCoexist(t *testing.T) {
	repo := newMemRepo()
	chairs := repo.add(domain.VenueFeature{Code: "kids_chairs", Name: "Детские стульчики", IsActive: true})
	childFree := repo.add(domain.VenueFeature{Code: "child_free", Name: "Без детей", IsActive: true})
	u := newUC(repo, &memPerms{allow: true})

	set, err := u.SetForRestaurant(context.Background(), venueStaff(), uuid.New(),
		[]uuid.UUID{chairs.ID, childFree.ID})
	if err != nil {
		t.Fatalf("assigning both = %v, want success: the server does not police meaning", err)
	}
	if len(set) != 2 {
		t.Errorf("set = %+v, want both features kept", set)
	}
}

// TestSetIsDedupedAndCapped: a repeated id is the caller's noise, not a second
// row; an unbounded set turns the AND-filter into an arbitrarily long chain.
func TestSetIsDedupedAndCapped(t *testing.T) {
	repo := newMemRepo()
	wifi := repo.add(domain.VenueFeature{Code: "wifi", Name: "Wi-Fi", IsActive: true})
	u := newUC(repo, &memPerms{allow: true})
	ctx := context.Background()

	set, err := u.SetForRestaurant(ctx, venueStaff(), uuid.New(), []uuid.UUID{wifi.ID, wifi.ID, wifi.ID})
	if err != nil {
		t.Fatalf("set with duplicates = %v", err)
	}
	if len(set) != 1 {
		t.Errorf("set = %+v, want the duplicate collapsed", set)
	}

	too := make([]uuid.UUID, 0, MaxFeaturesPerVenue+1)
	for i := 0; i <= MaxFeaturesPerVenue; i++ {
		f := repo.add(domain.VenueFeature{Code: "f" + uuid.New().String()[:8], Name: uuid.New().String(), IsActive: true})
		too = append(too, f.ID)
	}
	if _, err := u.SetForRestaurant(ctx, venueStaff(), uuid.New(), too); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("set over the cap = %v, want ErrValidation", err)
	}
}

// TestHiddenEntriesAreOnlyVisibleToThePlatform: a guest asking for hidden
// entries gets the active list rather than a 403 — the public route is
// anonymous by design, and the hidden ones are simply not their business.
func TestHiddenEntriesAreOnlyVisibleToThePlatform(t *testing.T) {
	repo := newMemRepo()
	repo.add(domain.VenueFeature{Code: "wifi", Name: "Wi-Fi", IsActive: true})
	repo.add(domain.VenueFeature{Code: "wine_list", Name: "Винная карта", IsActive: false})
	u := newUC(repo, &memPerms{allow: true})
	ctx := context.Background()

	guest, err := u.List(ctx, Actor{}, true)
	if err != nil {
		t.Fatalf("guest list: %v", err)
	}
	if len(guest) != 1 {
		t.Errorf("guest asking for hidden entries got %d, want only the active one", len(guest))
	}
	platform, err := u.List(ctx, admin(), true)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(platform) != 2 {
		t.Errorf("platform list got %d, want both", len(platform))
	}
}
