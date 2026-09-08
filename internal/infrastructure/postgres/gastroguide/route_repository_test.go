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

// The visibility rules of a route live in SQL — including the one that makes it
// differ from a collection (a dark venue loses its CARD, not its STOP) — so they
// are tested against a real Postgres. Reading the query and believing it is
// exactly how a wrong predicate ships.

func routeSetup(t *testing.T) (*pgxpool.Pool, *RouteRepository, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "gastro_route_points", "gastro_routes", "restaurants")
	return pool, NewRoutes(pool), context.Background()
}

type routeSeed struct {
	slug        string
	status      domain.GuideRouteStatus
	publishedAt *time.Time
	position    int
	city        *domain.City
}

func seedRoute(t *testing.T, pool *pgxpool.Pool, ctx context.Context, s routeSeed) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var city *string
	if s.city != nil {
		v := string(*s.city)
		city = &v
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO gastro_routes (id, slug, title, duration_label, status, published_at, position, city)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, s.slug, "Прогулка "+s.slug, "1 день · 4 точки",
		string(s.status), s.publishedAt, s.position, city)
	if err != nil {
		t.Fatalf("seed route %s: %v", s.slug, err)
	}
	return id
}

// addPoint inserts a stop straight into the table: these tests are about what
// the READ does with stored rows, so they must not depend on the writer.
func addPoint(t *testing.T, pool *pgxpool.Pool, ctx context.Context, routeID uuid.UUID,
	position int, kind domain.GuideRoutePointKind, venue *uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO gastro_route_points (id, route_id, position, kind, restaurant_id, title, address)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, routeID, position, string(kind), venue, title, "ул. Достык, 1")
	if err != nil {
		t.Fatalf("seed route point %q: %v", title, err)
	}
	return id
}

// A route walks in the editor's explicit order, not in insertion order and not
// in whatever order the heap happens to hold. For an itinerary this is the whole
// product: stop 3 before stop 1 is a different day.
func TestGetRoute_KeepsThePointOrder(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()

	venue := seedVenue(t, pool, ctx, "Daily Coffee", true)
	route := seedRoute(t, pool, ctx, routeSeed{
		slug: "classic-almaty", status: domain.GuideRoutePublished,
		publishedAt: ptrTime(now.Add(-time.Hour)), position: 1,
	})
	// Inserted 3, 1, 2 on purpose: only the position column can produce 1, 2, 3.
	third := addPoint(t, pool, ctx, route, 30, domain.GuideRoutePointRestaurant, &venue, "Вечер: Koktobe Terrace")
	first := addPoint(t, pool, ctx, route, 10, domain.GuideRoutePointRestaurant, &venue, "Утро: Daily Coffee")
	second := addPoint(t, pool, ctx, route, 20, domain.GuideRoutePointPlace, nil, "Парк 28 панфиловцев")

	got, err := repo.GetPublishedRouteBySlug(ctx, "classic-almaty", now)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	want := []uuid.UUID{first, second, third}
	if len(got.Points) != len(want) {
		t.Fatalf("points = %d, want %d", len(got.Points), len(want))
	}
	for i, id := range want {
		if got.Points[i].ID != id {
			t.Fatalf("point #%d = %s, want %s (order not respected)", i, got.Points[i].ID, id)
		}
	}
	if got.PointCount != 3 {
		t.Errorf("point_count = %d, want 3", got.PointCount)
	}
}

// A place stop is the reason routes are not collections: it has no venue at all
// and must still come back, with its own title and address.
func TestGetRoute_PlacePointHasNoVenueAndSurvives(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()
	route := seedRoute(t, pool, ctx, routeSeed{
		slug: "den-almatintsa", status: domain.GuideRoutePublished,
		publishedAt: ptrTime(now.Add(-time.Hour)),
	})
	addPoint(t, pool, ctx, route, 1, domain.GuideRoutePointPlace, nil, "Зелёный базар")

	got, err := repo.GetPublishedRouteBySlug(ctx, "den-almatintsa", now)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if len(got.Points) != 1 {
		t.Fatalf("points = %d, want 1", len(got.Points))
	}
	p := got.Points[0]
	if p.Kind != domain.GuideRoutePointPlace {
		t.Errorf("kind = %q, want place", p.Kind)
	}
	if p.Venue != nil {
		t.Errorf("venue = %+v, want nil for a place stop", p.Venue)
	}
	if p.Title != "Зелёный базар" || p.Address == "" {
		t.Errorf("place stop lost its own content: title %q, address %q", p.Title, p.Address)
	}
}

// THE DECISION THIS TEST PINS DOWN: when a venue is deactivated, the stop STAYS
// and only its venue card disappears.
//
// A collection drops such a venue from its list entirely — a list stays correct
// when it shrinks. An itinerary does not: «1 день · 4 точки» with three stops
// contradicts itself, and stop 2 of 5 vanishing leaves a walk from a coffee shop
// to a viewpoint with no explanation of what happens in between. The stop keeps
// its editorial title, text, photo and coordinates; what it loses is the card
// the guest could tap to book a table that is not bookable.
func TestGetRoute_DeactivatedVenueKeepsTheStopAndDropsTheCard(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()

	live := seedVenue(t, pool, ctx, "Chaihana Palau", true)
	dark := seedVenue(t, pool, ctx, "Abay", false)
	route := seedRoute(t, pool, ctx, routeSeed{
		slug: "classic-almaty", status: domain.GuideRoutePublished,
		publishedAt: ptrTime(now.Add(-time.Hour)),
	})
	addPoint(t, pool, ctx, route, 1, domain.GuideRoutePointRestaurant, &live, "Обед: Chaihana Palau")
	addPoint(t, pool, ctx, route, 2, domain.GuideRoutePointRestaurant, &dark, "Ужин: Abay")

	got, err := repo.GetPublishedRouteBySlug(ctx, "classic-almaty", now)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if len(got.Points) != 2 {
		t.Fatalf("points = %d, want 2 — a dark venue must not remove the stop", len(got.Points))
	}
	if got.PointCount != 2 {
		t.Errorf("point_count = %d, want 2", got.PointCount)
	}
	if got.Points[0].Venue == nil {
		t.Fatalf("stop 1 lost its venue card, but its venue is active")
	}
	if got.Points[0].Venue.ID != live || !got.Points[0].Venue.IsActive {
		t.Errorf("stop 1 venue = %+v, want the active %s", got.Points[0].Venue, live)
	}
	second := got.Points[1]
	if second.Venue != nil {
		t.Errorf("stop 2 venue = %+v, want nil: a guest must not be sent to a venue they cannot open", second.Venue)
	}
	if second.Title != "Ужин: Abay" {
		t.Errorf("stop 2 title = %q, want its own editorial title", second.Title)
	}
	// The link itself is untouched: deactivation is routinely temporary and the
	// editor must not lose the curation to it.
	if second.RestaurantID == nil || *second.RestaurantID != dark {
		t.Errorf("stop 2 restaurant_id = %v, want the link kept (%s)", second.RestaurantID, dark)
	}
}

// A venue stop carries everything a card needs, resolved server-side: the client
// renders the whole route from ONE response and never fans out per stop.
func TestGetRoute_VenueCardIsResolvedServerSide(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()

	venue := seedVenue(t, pool, ctx, "Daily Coffee", true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_images (id, restaurant_id, image_url, is_primary)
		 VALUES ($1, $2, $3, true)`,
		uuid.New(), venue, "https://cdn.example/daily.jpg"); err != nil {
		t.Fatalf("seed venue image: %v", err)
	}
	route := seedRoute(t, pool, ctx, routeSeed{
		slug: "classic-almaty", status: domain.GuideRoutePublished,
		publishedAt: ptrTime(now.Add(-time.Hour)),
	})
	addPoint(t, pool, ctx, route, 1, domain.GuideRoutePointRestaurant, &venue, "Утро: Daily Coffee")

	got, err := repo.GetPublishedRouteBySlug(ctx, "classic-almaty", now)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	card := got.Points[0].Venue
	if card == nil {
		t.Fatalf("venue card missing")
	}
	if card.ID != venue {
		t.Errorf("venue id = %s, want %s", card.ID, venue)
	}
	if card.Name != "Daily Coffee" {
		t.Errorf("venue name = %q, want %q", card.Name, "Daily Coffee")
	}
	// The seed writes CityAstana; assert positively so a mismatched city
	// constant fails loudly instead of silently matching an empty string.
	if card.City != domain.CityAstana {
		t.Errorf("venue city = %q, want %q", card.City, domain.CityAstana)
	}
	if card.PrimaryImageURL == nil || *card.PrimaryImageURL != "https://cdn.example/daily.jpg" {
		t.Errorf("venue image = %v, want the primary catalog image", card.PrimaryImageURL)
	}
}

// Only the route's OWN state decides whether a guest sees it. A draft, an
// archived route and one scheduled for tomorrow are all equally absent, and an
// unknown slug is the same 404 as a draft — otherwise the slug of an
// unannounced route leaks.
func TestRoutes_OnlyLiveOnesAreVisible(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()

	live := seedRoute(t, pool, ctx, routeSeed{
		slug: "live", status: domain.GuideRoutePublished, publishedAt: ptrTime(now.Add(-time.Hour)), position: 1})
	addPoint(t, pool, ctx, live, 1, domain.GuideRoutePointPlace, nil, "Кок-Тобе")
	for _, s := range []routeSeed{
		{slug: "draft", status: domain.GuideRouteDraft, position: 2},
		{slug: "archived", status: domain.GuideRouteArchived, publishedAt: ptrTime(now.Add(-time.Hour)), position: 3},
		{slug: "tomorrow", status: domain.GuideRoutePublished, publishedAt: ptrTime(now.Add(time.Hour)), position: 4},
	} {
		id := seedRoute(t, pool, ctx, s)
		addPoint(t, pool, ctx, id, 1, domain.GuideRoutePointPlace, nil, "Точка")
	}

	items, total, err := repo.ListPublishedRoutes(ctx, domain.GastroRouteFilter{}, now)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("listed %d routes (total %d), want exactly the live one", len(items), total)
	}
	if items[0].Slug != "live" {
		t.Fatalf("listed %q, want %q", items[0].Slug, "live")
	}

	for _, slug := range []string{"draft", "archived", "tomorrow", "no-such-route"} {
		_, err := repo.GetPublishedRouteBySlug(ctx, slug, now)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("get %q: err = %v, want ErrNotFound", slug, err)
		}
	}
}

// The city filter answers "what would a guest in this city see": routes pinned
// to that city PLUS the city-agnostic ones, exactly like the collections.
func TestListRoutes_CityFilterKeepsTheCityAgnosticOnes(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()
	almaty, astana := domain.CityAlmaty, domain.CityAstana

	for _, s := range []routeSeed{
		{slug: "almaty-walk", status: domain.GuideRoutePublished, publishedAt: ptrTime(now.Add(-time.Hour)), position: 1, city: &almaty},
		{slug: "astana-walk", status: domain.GuideRoutePublished, publishedAt: ptrTime(now.Add(-time.Hour)), position: 2, city: &astana},
		{slug: "anywhere", status: domain.GuideRoutePublished, publishedAt: ptrTime(now.Add(-time.Hour)), position: 3},
	} {
		id := seedRoute(t, pool, ctx, s)
		addPoint(t, pool, ctx, id, 1, domain.GuideRoutePointPlace, nil, "Точка")
	}

	items, total, err := repo.ListPublishedRoutes(ctx, domain.GastroRouteFilter{City: &almaty}, now)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	got := map[string]bool{}
	for _, rt := range items {
		got[rt.Slug] = true
	}
	if total != 2 || !got["almaty-walk"] || !got["anywhere"] {
		t.Fatalf("almaty listing = %v (total %d), want almaty-walk + anywhere", got, total)
	}
	if got["astana-walk"] {
		t.Errorf("astana-walk leaked into the almaty listing")
	}
}

// A deleted venue clears the link and KEEPS the stop (ON DELETE SET NULL). The
// alternative — CASCADE, which the collections use — would silently shorten an
// itinerary whose duration label still promises four stops.
func TestGetRoute_DeletedVenueKeepsTheStop(t *testing.T) {
	pool, repo, ctx := routeSetup(t)
	now := time.Now()

	venue := seedVenue(t, pool, ctx, "Скоро удалим", true)
	route := seedRoute(t, pool, ctx, routeSeed{
		slug: "classic-almaty", status: domain.GuideRoutePublished,
		publishedAt: ptrTime(now.Add(-time.Hour)),
	})
	addPoint(t, pool, ctx, route, 1, domain.GuideRoutePointRestaurant, &venue, "Утро: кофе")

	if _, err := pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, venue); err != nil {
		t.Fatalf("delete venue: %v", err)
	}

	got, err := repo.GetPublishedRouteBySlug(ctx, "classic-almaty", now)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if len(got.Points) != 1 {
		t.Fatalf("points = %d, want the stop to survive its venue", len(got.Points))
	}
	if got.Points[0].RestaurantID != nil || got.Points[0].Venue != nil {
		t.Errorf("stop still references the deleted venue: %+v", got.Points[0])
	}
	if got.Points[0].Title != "Утро: кофе" {
		t.Errorf("stop lost its own title: %q", got.Points[0].Title)
	}
}
