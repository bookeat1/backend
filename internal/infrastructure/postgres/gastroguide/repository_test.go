package gastroguide

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// The visibility rules of the guide live in SQL, so they are tested against a
// real Postgres — reading the query and believing it is exactly how a wrong
// predicate ships.

func setup(t *testing.T) (*pgxpool.Pool, *Repository, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool,
		"gastroguide_collection_venues", "gastroguide_collection_categories",
		"gastroguide_collections", "gastroguide_categories", "restaurants")
	return pool, New(pool), context.Background()
}

func seedVenue(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, name, string(domain.CityAstana), string(domain.PriceMid), active)
	if err != nil {
		t.Fatalf("seed venue %s: %v", name, err)
	}
	return id
}

type collectionSeed struct {
	slug        string
	status      domain.GuideCollectionStatus
	publishedAt *time.Time
	position    int
	city        *domain.City
}

func seedCollection(t *testing.T, pool *pgxpool.Pool, ctx context.Context, s collectionSeed) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var city *string
	if s.city != nil {
		v := string(*s.city)
		city = &v
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collections (id, slug, title, status, published_at, position, city)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, s.slug, "Подборка "+s.slug, string(s.status), s.publishedAt, s.position, city)
	if err != nil {
		t.Fatalf("seed collection %s: %v", s.slug, err)
	}
	return id
}

func addVenue(t *testing.T, pool *pgxpool.Pool, ctx context.Context, collectionID, venueID uuid.UUID, position int) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collection_venues (collection_id, restaurant_id, position)
		 VALUES ($1, $2, $3)`, collectionID, venueID, position)
	if err != nil {
		t.Fatalf("add venue to collection: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// A collection lists its venues in the editor's explicit order, not in
// insertion order and not in whatever order the heap happens to hold.
func TestGetCollection_RespectsEditorialVenueOrder(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	third := seedVenue(t, pool, ctx, "Третий", true)
	first := seedVenue(t, pool, ctx, "Первый", true)
	second := seedVenue(t, pool, ctx, "Второй", true)

	col := seedCollection(t, pool, ctx, collectionSeed{
		slug: "breakfasts", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	// Inserted 3, 1, 2 on purpose: only the position column can produce 1, 2, 3.
	addVenue(t, pool, ctx, col, third, 30)
	addVenue(t, pool, ctx, col, first, 10)
	addVenue(t, pool, ctx, col, second, 20)

	got, err := repo.GetPublishedCollectionBySlug(ctx, "breakfasts", now)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	want := []uuid.UUID{first, second, third}
	if len(got.Venues) != len(want) {
		t.Fatalf("venues = %d, want %d", len(got.Venues), len(want))
	}
	for i, id := range want {
		if got.Venues[i].RestaurantID != id {
			t.Fatalf("venue #%d = %s, want %s (order not respected)", i, got.Venues[i].RestaurantID, id)
		}
	}
}

// Collections are listed in the editor's order too.
func TestListCollections_RespectsEditorialOrder(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	venue := seedVenue(t, pool, ctx, "Место", true)

	for _, s := range []struct {
		slug string
		pos  int
	}{{"third", 30}, {"first", 10}, {"second", 20}} {
		id := seedCollection(t, pool, ctx, collectionSeed{
			slug: s.slug, status: domain.GuideCollectionPublished,
			publishedAt: ptrTime(now.Add(-time.Hour)), position: s.pos,
		})
		addVenue(t, pool, ctx, id, venue, 1)
	}

	items, total, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	for i, want := range []string{"first", "second", "third"} {
		if items[i].Slug != want {
			t.Fatalf("collection #%d = %s, want %s", i, items[i].Slug, want)
		}
	}
}

// A collection an editor has not published yet does not exist for a guest —
// neither in the listing nor by its own slug, and the slug answers exactly the
// same way an unknown one does.
func TestUnpublishedCollectionIsInvisibleToGuest(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	venue := seedVenue(t, pool, ctx, "Место", true)

	draft := seedCollection(t, pool, ctx, collectionSeed{
		slug: "draft-one", status: domain.GuideCollectionDraft, position: 1,
	})
	archived := seedCollection(t, pool, ctx, collectionSeed{
		slug: "archived-one", status: domain.GuideCollectionArchived,
		publishedAt: ptrTime(now.Add(-48 * time.Hour)), position: 2,
	})
	// Published, but scheduled for tomorrow: also not live yet.
	scheduled := seedCollection(t, pool, ctx, collectionSeed{
		slug: "tomorrow", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(24 * time.Hour)), position: 3,
	})
	live := seedCollection(t, pool, ctx, collectionSeed{
		slug: "live-one", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 4,
	})
	for _, id := range []uuid.UUID{draft, archived, scheduled, live} {
		addVenue(t, pool, ctx, id, venue, 1)
	}

	items, total, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Slug != "live-one" {
		t.Fatalf("listing leaked non-live collections: total=%d items=%+v", total, items)
	}

	for _, slug := range []string{"draft-one", "archived-one", "tomorrow", "no-such-slug"} {
		_, err := repo.GetPublishedCollectionBySlug(ctx, slug, now)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("get %q: err = %v, want ErrNotFound", slug, err)
		}
	}
}

// A venue that has been deactivated is unreachable in the app, so a collection
// must not offer it — and must not count it either.
func TestInactiveVenueIsNotShownAndNotCounted(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	active := seedVenue(t, pool, ctx, "Работает", true)
	inactive := seedVenue(t, pool, ctx, "Отключён", false)

	col := seedCollection(t, pool, ctx, collectionSeed{
		slug: "kids", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	addVenue(t, pool, ctx, col, inactive, 10)
	addVenue(t, pool, ctx, col, active, 20)

	got, err := repo.GetPublishedCollectionBySlug(ctx, "kids", now)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if len(got.Venues) != 1 || got.Venues[0].RestaurantID != active {
		t.Fatalf("inactive venue leaked into the collection: %+v", got.Venues)
	}
	if got.VenueCount != 1 {
		t.Fatalf("venue_count = %d, want 1 (the count must match what the guest can open)", got.VenueCount)
	}

	items, _, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].VenueCount != 1 {
		t.Fatalf("listing venue_count wrong: %+v", items)
	}
}

// Every venue of a live collection being deactivated empties the collection but
// does not resurrect it as a broken card in the listing.
func TestCollectionWithNoVisibleVenueDropsOutOfTheListing(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	inactive := seedVenue(t, pool, ctx, "Отключён", false)

	col := seedCollection(t, pool, ctx, collectionSeed{
		slug: "empty-now", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	addVenue(t, pool, ctx, col, inactive, 1)

	items, total, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("an empty collection stayed in the listing: %+v", items)
	}

	// Its own page still answers: the guest followed a real link.
	got, err := repo.GetPublishedCollectionBySlug(ctx, "empty-now", now)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if len(got.Venues) != 0 || got.VenueCount != 0 {
		t.Fatalf("expected an empty but existing collection, got %+v", got)
	}
}

// The same venue may be curated into several collections — that is the point of
// an editorial guide, and the schema must not force an editor to choose.
func TestVenueMayBelongToSeveralCollections(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	venue := seedVenue(t, pool, ctx, "Универсал", true)

	for _, slug := range []string{"kids", "breakfasts"} {
		id := seedCollection(t, pool, ctx, collectionSeed{
			slug: slug, status: domain.GuideCollectionPublished,
			publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
		})
		addVenue(t, pool, ctx, id, venue, 1)
	}

	for _, slug := range []string{"kids", "breakfasts"} {
		got, err := repo.GetPublishedCollectionBySlug(ctx, slug, now)
		if err != nil {
			t.Fatalf("get %s: %v", slug, err)
		}
		if len(got.Venues) != 1 || got.Venues[0].RestaurantID != venue {
			t.Fatalf("collection %s lost its venue: %+v", slug, got.Venues)
		}
	}
}

// Two venues cannot claim the same slot in one collection: the order is a
// constraint, not a suggestion. The constraint is deferred, so a renumbering
// transaction may pass through a colliding intermediate state.
func TestVenuePositionIsUniqueWithinACollection(t *testing.T) {
	pool, _, ctx := setup(t)
	now := time.Now()
	a := seedVenue(t, pool, ctx, "A", true)
	b := seedVenue(t, pool, ctx, "B", true)
	col := seedCollection(t, pool, ctx, collectionSeed{
		slug: "order", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	addVenue(t, pool, ctx, col, a, 1)

	if _, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collection_venues (collection_id, restaurant_id, position)
		 VALUES ($1, $2, 1)`, col, b); err == nil {
		t.Fatal("two venues were allowed to share position 1")
	}

	// A swap inside one transaction must be accepted (deferred constraint).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO gastroguide_collection_venues (collection_id, restaurant_id, position)
		 VALUES ($1, $2, 2)`, col, b); err != nil {
		t.Fatalf("insert second venue: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE gastroguide_collection_venues SET position = 2 WHERE collection_id = $1 AND restaurant_id = $2`,
		col, a); err != nil {
		t.Fatalf("intermediate collision was rejected before commit: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE gastroguide_collection_venues SET position = 1 WHERE collection_id = $1 AND restaurant_id = $2`,
		col, b); err != nil {
		t.Fatalf("second half of the swap: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit swap: %v", err)
	}
}

// A city-pinned collection is shown in its own city only; a city-agnostic one is
// shown everywhere.
func TestCityFilter(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	venue := seedVenue(t, pool, ctx, "Место", true)
	astana, almaty := domain.CityAstana, domain.CityAlmaty

	for _, s := range []collectionSeed{
		{slug: "astana-only", city: &astana, position: 1},
		{slug: "almaty-only", city: &almaty, position: 2},
		{slug: "everywhere", city: nil, position: 3},
	} {
		s.status = domain.GuideCollectionPublished
		s.publishedAt = ptrTime(now.Add(-time.Hour))
		id := seedCollection(t, pool, ctx, s)
		addVenue(t, pool, ctx, id, venue, 1)
	}

	items, _, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{City: &astana}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, c := range items {
		got[c.Slug] = true
	}
	if !got["astana-only"] || !got["everywhere"] || got["almaty-only"] {
		t.Fatalf("city filter wrong: %v", got)
	}
}

// Rubrics: only active ones, only those that currently hold a live collection,
// in their own order; and the category filter narrows the listing.
func TestCategories(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	venue := seedVenue(t, pool, ctx, "Место", true)

	seedCategory := func(slug string, position int, active bool) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO gastroguide_categories (id, slug, title, position, is_active)
			 VALUES ($1, $2, $3, $4, $5)`, id, slug, "Рубрика "+slug, position, active); err != nil {
			t.Fatalf("seed category: %v", err)
		}
		return id
	}
	link := func(collection, category uuid.UUID) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO gastroguide_collection_categories (collection_id, category_id, position)
			 VALUES ($1, $2, 1)`, collection, category); err != nil {
			t.Fatalf("link collection to category: %v", err)
		}
	}

	breakfasts := seedCategory("breakfasts", 20, true)
	kids := seedCategory("kids", 10, true)
	retired := seedCategory("retired", 5, false)
	empty := seedCategory("empty", 1, true)

	liveCol := seedCollection(t, pool, ctx, collectionSeed{
		slug: "live", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	addVenue(t, pool, ctx, liveCol, venue, 1)
	draftCol := seedCollection(t, pool, ctx, collectionSeed{
		slug: "draft", status: domain.GuideCollectionDraft, position: 2,
	})
	addVenue(t, pool, ctx, draftCol, venue, 1)

	link(liveCol, breakfasts)
	link(liveCol, kids)
	link(liveCol, retired)
	link(draftCol, empty)

	cats, err := repo.ListCategories(ctx, nil, now)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	var slugs []string
	for _, c := range cats {
		slugs = append(slugs, c.Slug)
	}
	if len(slugs) != 2 || slugs[0] != "kids" || slugs[1] != "breakfasts" {
		t.Fatalf("categories = %v, want [kids breakfasts] (active, non-empty, in order)", slugs)
	}

	kidsSlug := "kids"
	items, total, err := repo.ListPublishedCollections(ctx,
		domain.GuideCollectionFilter{CategorySlug: &kidsSlug}, now)
	if err != nil {
		t.Fatalf("list by category: %v", err)
	}
	if total != 1 || items[0].Slug != "live" {
		t.Fatalf("category filter wrong: total=%d items=%+v", total, items)
	}
	// The collection reports its rubrics, and a deactivated rubric is not one.
	if len(items[0].CategorySlugs) != 2 {
		t.Fatalf("category_slugs = %v, want the two active ones", items[0].CategorySlugs)
	}

	noneSlug := "no-such-category"
	_, total, err = repo.ListPublishedCollections(ctx,
		domain.GuideCollectionFilter{CategorySlug: &noneSlug}, now)
	if err != nil || total != 0 {
		t.Fatalf("unknown category: total=%d err=%v", total, err)
	}
}
