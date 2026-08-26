package story

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// mkRestaurant inserts a restaurant row and returns its id.
func mkRestaurant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,$2,'Алматы','₸')`,
		id, name); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

// TestCreateAndGetByID: Create writes the row and populates created_at; GetByID
// reads it back regardless of is_active.
func TestCreateAndGetByID(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	rid := mkRestaurant(t, ctx, pool, "A")
	cap := "Летнее меню"
	s := &domain.Story{RestaurantID: rid, ImageURL: "https://cdn/a.jpg", Caption: &cap, SortOrder: 3, IsActive: false}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.ID == uuid.Nil || s.CreatedAt.IsZero() {
		t.Fatalf("Create must populate id and created_at, got %+v", s)
	}
	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ImageURL != s.ImageURL || got.Caption == nil || *got.Caption != cap || got.SortOrder != 3 || got.IsActive {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestCreateUnknownRestaurantIsNotFound: an FK violation maps to ErrNotFound,
// not a raw driver error.
func TestCreateUnknownRestaurantIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	err := repo.Create(ctx, &domain.Story{RestaurantID: uuid.New(), ImageURL: "https://cdn/a.jpg"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown restaurant must map to ErrNotFound, got %v", err)
	}
}

// TestGetByIDMissing: an absent id is ErrNotFound.
func TestGetByIDMissing(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	repo := New(pool)
	if _, err := repo.GetByID(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing story must be ErrNotFound, got %v", err)
	}
}

// TestListByRestaurantIncludesInactive: the admin list returns inactive cards
// too, in display order, and excludes other venues.
func TestListByRestaurantIncludesInactive(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	ridA := mkRestaurant(t, ctx, pool, "A")
	ridB := mkRestaurant(t, ctx, pool, "B")
	mustCreate(t, ctx, repo, ridA, "https://cdn/a1.jpg", 1, true)
	mustCreate(t, ctx, repo, ridA, "https://cdn/a0.jpg", 0, false) // inactive, sorts first
	mustCreate(t, ctx, repo, ridB, "https://cdn/b.jpg", 0, true)

	got, err := repo.ListByRestaurant(ctx, ridA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("admin list must include the inactive card and exclude venue B, got %d", len(got))
	}
	if got[0].ImageURL != "https://cdn/a0.jpg" || got[0].IsActive {
		t.Fatalf("order/inactive mismatch: %+v", got[0])
	}
}

// TestUpdateScopedToRestaurant: an id belonging to another restaurant does not
// match and maps to ErrNotFound; the row for the correct restaurant updates.
func TestUpdateScopedToRestaurant(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	ridA := mkRestaurant(t, ctx, pool, "A")
	ridOther := mkRestaurant(t, ctx, pool, "Other")
	s := mustCreate(t, ctx, repo, ridA, "https://cdn/a.jpg", 0, true)

	// Wrong restaurant scope: no rows match.
	wrong := *s
	wrong.RestaurantID = ridOther
	wrong.ImageURL = "https://cdn/hacked.jpg"
	if err := repo.Update(ctx, &wrong); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update under the wrong restaurant must be ErrNotFound, got %v", err)
	}
	// Correct scope: applies.
	s.ImageURL = "https://cdn/new.jpg"
	s.IsActive = false
	if err := repo.Update(ctx, s); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := repo.GetByID(ctx, s.ID)
	if got.ImageURL != "https://cdn/new.jpg" || got.IsActive {
		t.Fatalf("update did not apply: %+v", got)
	}
}

// TestDeleteScopedToRestaurant: a delete under the wrong restaurant is
// ErrNotFound and leaves the row; under the right one it removes it.
func TestDeleteScopedToRestaurant(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	ridA := mkRestaurant(t, ctx, pool, "A")
	ridOther := mkRestaurant(t, ctx, pool, "Other")
	s := mustCreate(t, ctx, repo, ridA, "https://cdn/a.jpg", 0, true)

	if err := repo.Delete(ctx, s.ID, ridOther); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete under the wrong restaurant must be ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByID(ctx, s.ID); err != nil {
		t.Fatalf("the row must still exist after a cross-tenant delete, got %v", err)
	}
	if err := repo.Delete(ctx, s.ID, ridA); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, s.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the row must be gone, got %v", err)
	}
}

// TestReorderRewritesSortOrderAndIgnoresForeign: the unnest-with-ordinality
// UPDATE renumbers this venue's cards to their list position and leaves another
// venue's card untouched even when its id is slipped into the list.
func TestReorderRewritesSortOrderAndIgnoresForeign(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	ridA := mkRestaurant(t, ctx, pool, "A")
	ridB := mkRestaurant(t, ctx, pool, "B")
	a := mustCreate(t, ctx, repo, ridA, "https://cdn/a.jpg", 0, true)
	b := mustCreate(t, ctx, repo, ridA, "https://cdn/b.jpg", 1, true)
	c := mustCreate(t, ctx, repo, ridA, "https://cdn/c.jpg", 2, true)
	foreign := mustCreate(t, ctx, repo, ridB, "https://cdn/f.jpg", 9, true)

	// New order for A: c, a, b. foreign.ID is slipped in but belongs to B.
	if err := repo.Reorder(ctx, ridA, []uuid.UUID{c.ID, foreign.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	// c→0, a→2, b→3 (positions in the list, foreign at index 1 is skipped for A).
	if pos := sortOrderOf(t, ctx, repo, c.ID); pos != 0 {
		t.Fatalf("c sort_order = %d, want 0", pos)
	}
	if pos := sortOrderOf(t, ctx, repo, a.ID); pos != 2 {
		t.Fatalf("a sort_order = %d, want 2", pos)
	}
	if pos := sortOrderOf(t, ctx, repo, b.ID); pos != 3 {
		t.Fatalf("b sort_order = %d, want 3", pos)
	}
	if pos := sortOrderOf(t, ctx, repo, foreign.ID); pos != 9 {
		t.Fatalf("a foreign venue's card must be untouched, sort_order = %d, want 9", pos)
	}
}

// TestReorderEmptyIsNoop: an empty list changes nothing.
func TestReorderEmptyIsNoop(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	rid := mkRestaurant(t, ctx, pool, "A")
	s := mustCreate(t, ctx, repo, rid, "https://cdn/a.jpg", 5, true)
	if err := repo.Reorder(ctx, rid, nil); err != nil {
		t.Fatalf("reorder empty: %v", err)
	}
	if pos := sortOrderOf(t, ctx, repo, s.ID); pos != 5 {
		t.Fatalf("empty reorder changed sort_order to %d, want 5", pos)
	}
}

func mustCreate(t *testing.T, ctx context.Context, repo *Repository, rid uuid.UUID, url string, sortOrder int, active bool) *domain.Story {
	t.Helper()
	s := &domain.Story{RestaurantID: rid, ImageURL: url, SortOrder: sortOrder, IsActive: active}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	return s
}

func sortOrderOf(t *testing.T, ctx context.Context, repo *Repository, id uuid.UUID) int {
	t.Helper()
	s, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get %v: %v", id, err)
	}
	return s.SortOrder
}

// TestActionURLRoundTrips: the external link a tap on the story follows survives
// Create → GetByID → Update, and it is a DIFFERENT column from image_url (the
// picture's address). Both lists read it back too, so the public and admin
// surfaces see the same link.
func TestActionURLRoundTrips(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	rid := mkRestaurant(t, ctx, pool, "A")
	link := "https://book-eat.com/promo"
	s := &domain.Story{RestaurantID: rid, ImageURL: "https://cdn/a.jpg", ActionURL: &link, IsActive: true}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ActionURL == nil || *got.ActionURL != link {
		t.Fatalf("action_url must round-trip, got %v", got.ActionURL)
	}
	if got.ImageURL != "https://cdn/a.jpg" {
		t.Fatalf("image_url must stay the picture's address, got %q", got.ImageURL)
	}

	list, err := repo.ListActiveByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ActionURL == nil || *list[0].ActionURL != link {
		t.Fatalf("the public read must carry the link: %+v", list)
	}

	// Clearing the link (nil) is a reachable state, and it must not disturb the
	// picture.
	got.ActionURL = nil
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.ActionURL != nil {
		t.Fatalf("the link must be clearable, got %v", *after.ActionURL)
	}
	if after.ImageURL != "https://cdn/a.jpg" {
		t.Fatalf("clearing the link must not touch image_url, got %q", after.ImageURL)
	}
}

// TestActionURLCheckConstraintRefusesDangerousLink: the CHECK added in 0086 is
// the SECOND line of defence — the first is domain.ValidateExternalActionURL.
// This test writes straight past the application (a manual UPDATE, a future
// import) and asserts the database itself refuses a javascript: link, a
// credentials-bearing URL and one carrying a smuggled newline.
func TestActionURLCheckConstraintRefusesDangerousLink(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()
	rid := mkRestaurant(t, ctx, pool, "A")

	for _, raw := range []string{
		"javascript:alert(1)",
		"java\nscript:alert(1)",
		"https://user:pass@book-eat.com/promo",
		"https://book-eat.com/ promo",
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO restaurant_stories (restaurant_id, image_url, action_url) VALUES ($1,'https://cdn/a.jpg',$2)`,
			rid, raw)
		if err == nil {
			t.Fatalf("the CHECK constraint must refuse %q", raw)
		}
	}

	// The same insert with a sane link succeeds — the constraint is not simply
	// rejecting everything.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_stories (restaurant_id, image_url, action_url) VALUES ($1,'https://cdn/a.jpg','https://book-eat.com/promo')`,
		rid); err != nil {
		t.Fatalf("a valid link must be accepted: %v", err)
	}
}
