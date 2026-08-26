package event

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// Whether an event with NO venue is expressible, visible and openable is
// decided in SQL — a nullable column, a LEFT JOIN and a COALESCE — so it is
// tested against real Postgres. A fake would only re-run the Go code that has
// no say in it.

// seedPlatformEvent writes an event nobody hosts, optionally pinned to a city.
func seedPlatformEvent(ctx context.Context, t *testing.T, repo *Repository, title string, city *string) uuid.UUID {
	t.Helper()
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	e := &domain.Event{
		Title: title, StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.EventPublished,
	}
	if city != nil {
		c := domain.City(*city)
		e.City = &c
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create platform event %s: %v", title, err)
	}
	return e.ID
}

// A platform event with no city of its own appears in EVERY city's Афиша, next
// to the venue-bound events that live there. Before this change the column was
// NOT NULL and the venue was inner-joined: the row could not exist, and if it
// had, the join would have dropped it.
func TestListPublicUpcoming_PlatformEventAppearsInEveryCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	almatyVenue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astanaVenue := seedVenueIn(ctx, t, pool, "Bistro Astana", domain.CityAstana)
	seedEventIn(ctx, t, repo, almatyVenue, "Ужин в Алматы", nil)
	seedEventIn(ctx, t, repo, astanaVenue, "Ужин в Астане", nil)
	seedPlatformEvent(ctx, t, repo, "Фестиваль платформы", nil)

	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Ужин в Алматы", "Фестиваль платформы")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)), "Ужин в Астане", "Фестиваль платформы")
}

// A platform event pinned to a city runs there and nowhere else — its own
// override is the only city it has.
func TestListPublicUpcoming_PlatformEventHonoursItsCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	astana := string(domain.CityAstana)
	seedPlatformEvent(ctx, t, repo, "Афиша Астаны", &astana)

	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)), "Афиша Астаны")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)))
}

// The venue block is a real optional: present for a venue-bound event, absent
// for a platform one. A consumer that assumed it was always there is the bug
// this asserts against.
func TestListPublicUpcoming_PlatformEventCarriesNoVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedEventIn(ctx, t, repo, venue, "Ужин у Bistro", nil)
	seedPlatformEvent(ctx, t, repo, "Фестиваль платформы", nil)

	items, _, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want both events", len(items))
	}
	for _, it := range items {
		switch it.Title {
		case "Ужин у Bistro":
			if it.Restaurant == nil || it.Restaurant.ID != venue || it.Restaurant.Name != "Bistro Almaty" {
				t.Fatalf("a venue-bound event must still carry its venue, got %+v", it.Restaurant)
			}
			if it.RestaurantID == nil || *it.RestaurantID != venue {
				t.Fatalf("restaurant_id = %v, want %s", it.RestaurantID, venue)
			}
		case "Фестиваль платформы":
			if it.Restaurant != nil {
				t.Fatalf("a platform event must carry no venue, got %+v", it.Restaurant)
			}
			if it.RestaurantID != nil {
				t.Fatalf("restaurant_id = %v, want nil", it.RestaurantID)
			}
		}
	}
}

// A deactivated venue still takes its events off the Афиша — the LEFT JOIN must
// not have loosened that. And the platform's own event, which has no venue to
// deactivate, stays.
func TestListPublicUpcoming_DeactivatedVenueStillHidesItsEvents(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedEventIn(ctx, t, repo, venue, "Ужин у Bistro", nil)
	seedPlatformEvent(ctx, t, repo, "Фестиваль платформы", nil)

	if _, err := pool.Exec(ctx, `UPDATE restaurants SET is_active = false WHERE id = $1`, venue); err != nil {
		t.Fatalf("deactivate venue: %v", err)
	}

	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Фестиваль платформы")
}

// The event's own page answers for both shapes, and applies the same visibility
// rule the listing does.
func TestGetPublicByID(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	bound := seedEventIn(ctx, t, repo, venue, "Ужин у Bistro", nil)
	platform := seedPlatformEvent(ctx, t, repo, "Фестиваль платформы", nil)

	got, err := repo.GetPublicByID(ctx, platform, time.Now())
	if err != nil {
		t.Fatalf("GetPublicByID(platform): %v", err)
	}
	if got.Restaurant != nil || got.RestaurantID != nil {
		t.Fatalf("a platform event must have no venue, got %+v", got.Restaurant)
	}

	got, err = repo.GetPublicByID(ctx, bound, time.Now())
	if err != nil {
		t.Fatalf("GetPublicByID(venue-bound): %v", err)
	}
	if got.Restaurant == nil || got.Restaurant.ID != venue {
		t.Fatalf("a venue-bound event must carry its venue, got %+v", got.Restaurant)
	}

	// A draft is invisible to a guest, whoever hosts it.
	draft := mkEvent(venue, domain.EventDraft, 24*time.Hour, 2*time.Hour)
	if err := repo.Create(ctx, draft); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := repo.GetPublicByID(ctx, draft.ID, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("draft: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetPublicByID(ctx, uuid.New(), time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

// The platform cabinet's listing sees the platform's own events and nothing
// else — the venue's stay with the venue.
func TestListPlatform(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedEventIn(ctx, t, repo, venue, "Ужин у Bistro", nil)
	seedPlatformEvent(ctx, t, repo, "Фестиваль платформы", nil)

	items, total, err := repo.ListPlatform(ctx, nil, 1, 20)
	if err != nil {
		t.Fatalf("ListPlatform: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Title != "Фестиваль платформы" {
		t.Fatalf("got %d items %+v, want only the platform's own", total, items)
	}

	// The venue's own cabinet listing is unchanged.
	own, ownTotal, err := repo.ListByRestaurant(ctx, venue, nil, 1, 20)
	if err != nil {
		t.Fatalf("ListByRestaurant: %v", err)
	}
	if ownTotal != 1 || len(own) != 1 || own[0].Title != "Ужин у Bistro" {
		t.Fatalf("got %d items %+v, want only the venue's own", ownTotal, own)
	}
}

// The action button round-trips through the database, and the two "no button"
// halves stay one state (both columns NULL).
func TestActionButtonRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	link := "https://tickets.kz/e/42"
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	e := &domain.Event{
		Title: "Концерт", StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.EventPublished,
		Action: &domain.EventAction{Label: "Купить билет", URL: &link},
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Action == nil || got.Action.Label != "Купить билет" || got.Action.URL == nil || *got.Action.URL != link {
		t.Fatalf("action = %+v, want the stored button", got.Action)
	}
	if got.Action.Target() != domain.EventActionTargetExternal {
		t.Fatalf("target = %q, want external", got.Action.Target())
	}

	// A button onto the event's OWN page: a label with no url.
	got.Action = &domain.EventAction{Label: "Подробнее"}
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Action == nil || got.Action.URL != nil || got.Action.Target() != domain.EventActionTargetEvent {
		t.Fatalf("action = %+v, want a button onto the event's own page", got.Action)
	}

	// Removing the button leaves no half-state behind.
	got.Action = nil
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Action != nil {
		t.Fatalf("action = %+v, want no button", got.Action)
	}
}

// The database refuses what the usecase refuses: a venue-less event that sells
// tickets. This is the last line of defence for the money path — the ticket
// would have no venue to settle to.
func TestPlatformEventCannotBeTicketed(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	_, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, ticketed)
		 VALUES ($1, NULL, 'Платный фестиваль', $2, $3, 'published', true)`,
		uuid.New(), start, start.Add(2*time.Hour))
	if err == nil {
		t.Fatal("the database accepted a ticketed event with no venue")
	}
	if !strings.Contains(err.Error(), "events_platform_not_ticketed") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}

// The scheme allowlist is enforced in the database too, so a write that bypasses
// the application (a manual UPDATE, a future import) cannot store a
// javascript: link.
func TestActionURLSchemeIsCheckedByTheDatabase(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	_, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, action_label, action_url)
		 VALUES ($1, NULL, 'Концерт', $2, $3, 'published', 'Купить', 'javascript:alert(1)')`,
		uuid.New(), start, start.Add(2*time.Hour))
	if err == nil {
		t.Fatal("the database accepted a javascript: action url")
	}
	if !strings.Contains(err.Error(), "events_action_url_scheme") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}

	// ...and a url without a label is not a drawable button.
	_, err = pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, action_url)
		 VALUES ($1, NULL, 'Концерт', $2, $3, 'published', 'https://tickets.kz/e/1')`,
		uuid.New(), start, start.Add(2*time.Hour))
	if err == nil {
		t.Fatal("the database accepted an action url with no label")
	}
	if !strings.Contains(err.Error(), "events_action_url_needs_label") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}
