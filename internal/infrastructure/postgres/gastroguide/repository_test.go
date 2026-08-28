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
	// kind is empty in most seeds: that is the point of migration 0092's
	// DEFAULT — a row written without one is a collection.
	kind domain.GuideCollectionKind
}

func seedCollection(t *testing.T, pool *pgxpool.Pool, ctx context.Context, s collectionSeed) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var city *string
	if s.city != nil {
		v := string(*s.city)
		city = &v
	}
	kind := s.kind
	if kind == "" {
		kind = domain.GuideKindCollection
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collections (id, slug, title, status, published_at, position, city, kind)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, s.slug, "Подборка "+s.slug, string(s.status), s.publishedAt, s.position, city, string(kind))
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

// Every venue of a live collection being deactivated empties it — and it STAYS
// in the listing. The guide is editorial content: the article is the payload,
// the venues are a bonus, and an editor who published it did not ask for it to
// be retracted the moment a restaurant is switched off.
func TestCollectionWithNoVisibleVenueStaysInTheListing(t *testing.T) {
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
	if total != 1 || len(items) != 1 || items[0].Slug != "empty-now" {
		t.Fatalf("an emptied collection was hidden: total=%d items=%+v", total, items)
	}
	if items[0].VenueCount != 0 {
		t.Fatalf("venue_count = %d, want 0 (the deactivated venue must not be counted)", items[0].VenueCount)
	}

	// Its own page answers with an empty venue list, not a 404.
	got, err := repo.GetPublishedCollectionBySlug(ctx, "empty-now", now)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if len(got.Venues) != 0 || got.VenueCount != 0 {
		t.Fatalf("expected an empty but existing collection, got %+v", got)
	}
}

// A collection that never had a venue at all — an article about places outside
// the catalog — is listed, is counted in the total and opens by its own slug.
// This is the state the listing used to swallow.
func TestCollectionWithNoVenuesAtAllIsListedAndOpens(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	seedCollection(t, pool, ctx, collectionSeed{
		slug: "no-venues", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})

	items, total, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Slug != "no-venues" {
		t.Fatalf("a venue-less collection was hidden: total=%d items=%+v", total, items)
	}
	if items[0].VenueCount != 0 {
		t.Fatalf("venue_count = %d, want 0", items[0].VenueCount)
	}

	got, err := repo.GetPublishedCollectionBySlug(ctx, "no-venues", now)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if got.Venues == nil {
		// nil is fine for the transport (it renders []), but the detail must at
		// least exist and know its own emptiness.
		t.Log("venues came back nil; the transport turns that into []")
	}
	if len(got.Venues) != 0 || got.VenueCount != 0 {
		t.Fatalf("expected an empty venue list, got %+v", got)
	}

	// The city filter still applies to a venue-less collection.
	almaty := domain.CityAlmaty
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_collections SET city = $1 WHERE slug = 'no-venues'`,
		string(domain.CityAstana)); err != nil {
		t.Fatalf("pin the collection to a city: %v", err)
	}
	_, total, err = repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{City: &almaty}, now)
	if err != nil {
		t.Fatalf("list by city: %v", err)
	}
	if total != 0 {
		t.Fatalf("an Astana collection leaked into the Almaty listing: total=%d", total)
	}
}

// Opening the listing to empty collections must not open it to unpublished
// ones: a draft, an archived collection and one scheduled for tomorrow stay
// invisible whether or not they hold a venue.
func TestVenuelessButUnpublishedCollectionsStayHidden(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	seedCollection(t, pool, ctx, collectionSeed{
		slug: "draft-empty", status: domain.GuideCollectionDraft, position: 1,
	})
	seedCollection(t, pool, ctx, collectionSeed{
		slug: "archived-empty", status: domain.GuideCollectionArchived,
		publishedAt: ptrTime(now.Add(-48 * time.Hour)), position: 2,
	})
	seedCollection(t, pool, ctx, collectionSeed{
		slug: "tomorrow-empty", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(24 * time.Hour)), position: 3,
	})

	items, total, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("unpublished venue-less collections leaked: total=%d items=%+v", total, items)
	}
	for _, slug := range []string{"draft-empty", "archived-empty", "tomorrow-empty"} {
		if _, err := repo.GetPublishedCollectionBySlug(ctx, slug, now); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("get %q: err = %v, want ErrNotFound", slug, err)
		}
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

// A block illustrated with an EVENT carries the event's own title and text, the
// venue's Instagram, and stays a link to the venue — the «Статья» design draws
// exactly that. Read against a real Postgres because every field here comes
// from a join, and a join that names a column wrong compiles perfectly.
func TestGetCollection_VenueBlockCarriesItsEventAndInstagram(t *testing.T) {
	pool, repo, ctx := setup(t)
	testdb.Truncate(t, pool, "events", "restaurant_social_links")

	venue := seedVenue(t, pool, ctx, "Mongol", true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_social_links (id, restaurant_id, type, url)
		 VALUES ($1, $2, 'instagram', 'https://instagram.com/mongol.almaty')`,
		uuid.New(), venue); err != nil {
		t.Fatalf("seed social link: %v", err)
	}

	eventID := uuid.New()
	starts := time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, description, starts_at, ends_at, status, cover_image_url)
		 VALUES ($1, $2, 'Коктейльная среда', 'Еженедельное событие', $3, $4, 'published', 'https://x/cover.jpg')`,
		eventID, venue, starts, starts.Add(3*time.Hour)); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	collection := seedCollection(t, pool, ctx, collectionSeed{
		slug: "mongol", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(time.Now().Add(-time.Hour)),
	})
	addVenue(t, pool, ctx, collection, venue, 1)
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_collection_venues SET event_id = $1
		 WHERE collection_id = $2 AND restaurant_id = $3`, eventID, collection, venue); err != nil {
		t.Fatalf("attach event to the block: %v", err)
	}

	got, err := repo.GetPublishedCollectionBySlug(ctx, "mongol", time.Now())
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if len(got.Venues) != 1 {
		t.Fatalf("venues = %d, want 1", len(got.Venues))
	}
	block := got.Venues[0]
	if block.Instagram != "https://instagram.com/mongol.almaty" {
		t.Fatalf("instagram = %q, want the venue's own link", block.Instagram)
	}
	if block.Highlight == nil {
		t.Fatal("block lost its event: highlight is nil")
	}
	if block.Highlight.Kind != domain.GuideHighlightEvent || block.Highlight.ID != eventID {
		t.Fatalf("highlight = %s/%s, want event/%s", block.Highlight.Kind, block.Highlight.ID, eventID)
	}
	if block.Highlight.Title != "Коктейльная среда" || block.Highlight.Description != "Еженедельное событие" {
		t.Fatalf("highlight text = %q / %q, want the event's own", block.Highlight.Title, block.Highlight.Description)
	}
	if !block.Highlight.StartsAt.Equal(starts) {
		t.Fatalf("highlight starts at %s, want %s", block.Highlight.StartsAt, starts)
	}
}

// A venue with NO social links and no attached event stays the plain card it
// was: an empty Instagram, not a stray value borrowed from another venue.
func TestGetCollection_PlainVenueBlockHasNoHighlightAndNoInstagram(t *testing.T) {
	pool, repo, ctx := setup(t)
	testdb.Truncate(t, pool, "events", "restaurant_social_links")

	venue := seedVenue(t, pool, ctx, "Без соцсетей", true)
	collection := seedCollection(t, pool, ctx, collectionSeed{
		slug: "plain", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(time.Now().Add(-time.Hour)),
	})
	addVenue(t, pool, ctx, collection, venue, 1)

	got, err := repo.GetPublishedCollectionBySlug(ctx, "plain", time.Now())
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if len(got.Venues) != 1 {
		t.Fatalf("venues = %d, want 1", len(got.Venues))
	}
	if got.Venues[0].Instagram != "" || got.Venues[0].Highlight != nil {
		t.Fatalf("plain block invented content: %+v", got.Venues[0])
	}
}

// A rubric whose only live collection holds no visible venue is still offered:
// tapping it opens an article, which is exactly what the guide is for. A rubric
// that holds only a draft is still not offered — that screen really is empty.
func TestCategories_RubricWithOnlyAnEmptyCollectionIsOffered(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	seedCategory := func(slug string, position int) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO gastroguide_categories (id, slug, title, position, is_active)
			 VALUES ($1, $2, $3, $4, true)`, id, slug, "Рубрика "+slug, position); err != nil {
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

	stories := seedCategory("stories", 10)
	onlyDraft := seedCategory("only-draft", 20)

	empty := seedCollection(t, pool, ctx, collectionSeed{
		slug: "story-no-venues", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	draft := seedCollection(t, pool, ctx, collectionSeed{
		slug: "still-writing", status: domain.GuideCollectionDraft, position: 2,
	})
	link(empty, stories)
	link(draft, onlyDraft)

	cats, err := repo.ListCategories(ctx, nil, now)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	var slugs []string
	for _, c := range cats {
		slugs = append(slugs, c.Slug)
	}
	if len(slugs) != 1 || slugs[0] != "stories" {
		t.Fatalf("categories = %v, want [stories] (empty-but-live counts, draft does not)", slugs)
	}

	// And the rubric filter actually returns that collection, so the chip does
	// not open into a blank screen.
	storiesSlug := "stories"
	items, total, err := repo.ListPublishedCollections(ctx,
		domain.GuideCollectionFilter{CategorySlug: &storiesSlug}, now)
	if err != nil {
		t.Fatalf("list by category: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Slug != "story-no-venues" {
		t.Fatalf("rubric filter lost the empty collection: total=%d items=%+v", total, items)
	}
}

// --- articles vs collections (migration 0092) ---

// The kind filter really partitions the table: neither listing can see the
// other's rows, and no filter returns both. Read against a real Postgres,
// because the whole split is one WHERE clause and a wrong one compiles fine.
func TestKindFilter_PartitionsTheListing(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	live := ptrTime(now.Add(-time.Hour))

	seedCollection(t, pool, ctx, collectionSeed{
		slug: "kazakh-cuisine", status: domain.GuideCollectionPublished,
		publishedAt: live, position: 1, kind: domain.GuideKindCollection,
	})
	seedCollection(t, pool, ctx, collectionSeed{
		slug: "gde-poest-s-rebenkom-v-almaty", status: domain.GuideCollectionPublished,
		publishedAt: live, position: 2, kind: domain.GuideKindArticle,
	})
	// A draft article: the kind filter must not resurrect it. Visibility stays
	// in SQL and no filter can widen it.
	seedCollection(t, pool, ctx, collectionSeed{
		slug: "draft-article", status: domain.GuideCollectionDraft,
		position: 3, kind: domain.GuideKindArticle,
	})

	collections, articles := domain.GuideKindCollection, domain.GuideKindArticle
	cases := []struct {
		name  string
		kind  *domain.GuideCollectionKind
		slugs []string
	}{
		{"collections only", &collections, []string{"kazakh-cuisine"}},
		{"articles only", &articles, []string{"gde-poest-s-rebenkom-v-almaty"}},
		{"no kind filter sees both", nil,
			[]string{"kazakh-cuisine", "gde-poest-s-rebenkom-v-almaty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := repo.ListPublishedCollections(ctx,
				domain.GuideCollectionFilter{Kind: tc.kind}, now)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if total != len(tc.slugs) {
				t.Fatalf("total = %d, want %d", total, len(tc.slugs))
			}
			var got []string
			for _, it := range items {
				got = append(got, it.Slug)
			}
			if len(got) != len(tc.slugs) {
				t.Fatalf("items = %v, want %v", got, tc.slugs)
			}
			for i := range got {
				if got[i] != tc.slugs[i] {
					t.Fatalf("items = %v, want %v", got, tc.slugs)
				}
			}
		})
	}
}

// The kind is read back on the row, so the transport does not have to infer it
// from the presence of rubrics.
func TestKind_IsScannedOnBothListingAndDetail(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()
	live := ptrTime(now.Add(-time.Hour))

	seedCollection(t, pool, ctx, collectionSeed{
		slug: "chto-proishodit", status: domain.GuideCollectionPublished,
		publishedAt: live, position: 1, kind: domain.GuideKindArticle,
	})

	items, _, err := repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{}, now)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: items=%d err=%v", len(items), err)
	}
	if items[0].Kind != domain.GuideKindArticle {
		t.Fatalf("listing kind = %q, want article", items[0].Kind)
	}

	// The detail read is kind-agnostic ON PURPOSE: a slug is unique across the
	// table and mobile deep-links already point articles at the collection
	// route. It must resolve, and it must say what it resolved.
	detail, err := repo.GetPublishedCollectionBySlug(ctx, "chto-proishodit", now)
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if detail.Kind != domain.GuideKindArticle {
		t.Fatalf("detail kind = %q, want article", detail.Kind)
	}
}

// The backfill rule of migration 0092, run against a real schema: a row with no
// rubric row at all becomes an article, a row with one stays a collection. This
// is the statement that decided the live 4/4 split, so it is tested as SQL and
// not as a paragraph in a migration comment.
func TestKindBackfillRule_RubriclessRowsBecomeArticles(t *testing.T) {
	pool, _, ctx := setup(t)

	catID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_categories (id, slug, title) VALUES ($1, 'kazakh-cuisine', 'Казахская')`,
		catID); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	withRubric := seedCollection(t, pool, ctx, collectionSeed{slug: "with-rubric", status: domain.GuideCollectionDraft})
	seedCollection(t, pool, ctx, collectionSeed{slug: "without-rubric", status: domain.GuideCollectionDraft, position: 2})
	if _, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collection_categories (collection_id, category_id, position)
		 VALUES ($1, $2, 1)`, withRubric, catID); err != nil {
		t.Fatalf("link rubric: %v", err)
	}

	// Both rows are collections right now — that is the column DEFAULT, which
	// is what migration 0092 relies on for the "has a rubric" half.
	var before int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM gastroguide_collections WHERE kind = 'collection'`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 2 {
		t.Fatalf("collections before backfill = %d, want 2 (the column DEFAULT)", before)
	}

	// The exact statement from the migration.
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_collections SET kind = 'article'
		 WHERE id NOT IN (SELECT collection_id FROM gastroguide_collection_categories)`); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	got := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT slug, kind FROM gastroguide_collections`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, kind string
		if err := rows.Scan(&slug, &kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[slug] = kind
	}
	if got["with-rubric"] != "collection" || got["without-rubric"] != "article" {
		t.Fatalf("backfill = %v, want with-rubric=collection without-rubric=article", got)
	}
}

// The rubric listing counts kind='collection' rows only. The usecase already
// refuses to give an article a rubric; this pins the SQL half of the invariant,
// so a bad backfill or a hand-written UPDATE cannot put an article into the
// guide's rubric navigation.
func TestCategories_AnArticleNeverMakesARubricNonEmpty(t *testing.T) {
	pool, repo, ctx := setup(t)
	now := time.Now()

	catID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_categories (id, slug, title, position, is_active)
		 VALUES ($1, 'kids', 'С детьми', 1, true)`, catID); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	article := seedCollection(t, pool, ctx, collectionSeed{
		slug: "gde-poest-s-rebenkom-v-almaty", status: domain.GuideCollectionPublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1, kind: domain.GuideKindArticle,
	})
	// Attached straight in SQL, bypassing the usecase — exactly the state the
	// application layer refuses to create.
	if _, err := pool.Exec(ctx,
		`INSERT INTO gastroguide_collection_categories (collection_id, category_id, position)
		 VALUES ($1, $2, 1)`, article, catID); err != nil {
		t.Fatalf("link rubric to article: %v", err)
	}

	cats, err := repo.ListCategories(ctx, nil, now)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	if len(cats) != 0 {
		t.Fatalf("categories = %+v, want none: only a COLLECTION makes a rubric non-empty", cats)
	}
}
