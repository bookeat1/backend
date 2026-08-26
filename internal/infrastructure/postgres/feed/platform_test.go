package feed

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	promorepo "backend-core/internal/infrastructure/postgres/promo"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// The main screen is the one place a nil venue can take the whole app down: the
// read model used to JOIN restaurants, and a card with no venue would have been
// silently dropped (best case) or crashed the scan (once the join became a LEFT
// one without COALESCE). These tests run against real Postgres for that reason.

// seedPlatformPromo inserts an approved, live promo that NOBODY runs but us.
func seedPlatformPromo(ctx context.Context, t *testing.T, pool sqltx.Querier,
	title string, city *domain.City, startsAt, endsAt time.Time) uuid.UUID {
	t.Helper()
	p := &domain.Promo{
		Title: title, Status: domain.PromoPublished, City: city,
		StartsAt: startsAt, EndsAt: endsAt,
	}
	if err := promorepo.New(pool).Create(ctx, p); err != nil {
		t.Fatalf("seed platform promo: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE promos SET feed_status = 'approved' WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("approve platform promo: %v", err)
	}
	return p.ID
}

// seedPlatformEvent inserts an approved, upcoming event that nobody hosts.
func seedPlatformEvent(ctx context.Context, t *testing.T, pool sqltx.Querier,
	title string, city *domain.City, startsAt, endsAt time.Time) uuid.UUID {
	t.Helper()
	e := &domain.Event{
		Title: title, Status: domain.EventPublished, City: city,
		StartsAt: startsAt, EndsAt: endsAt,
		TicketsRefundable:         domain.DefaultTicketRefundPolicy.Refundable,
		TicketRefundCutoffMinutes: domain.DefaultTicketRefundPolicy.CutoffMinutes,
	}
	if err := eventrepo.New(pool).Create(ctx, e); err != nil {
		t.Fatalf("seed platform event: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE events SET feed_status = 'approved' WHERE id = $1`, e.ID); err != nil {
		t.Fatalf("approve platform event: %v", err)
	}
	return e.ID
}

func titlesOf(items []domain.FeedItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func mustContain(t *testing.T, items []domain.FeedItem, titles ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.Title] = true
	}
	for _, want := range titles {
		if !seen[want] {
			t.Fatalf("main screen %v is missing %q", titlesOf(items), want)
		}
	}
}

// A platform card with no city reaches EVERY city's main screen, and does it
// without disturbing the venue-bound cards already there.
func TestListCandidates_PlatformCardRunsInEveryCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	almatyVenue := seedVenue(ctx, t, pool, "Almaty venue", activeVenue())
	astanaVenue := seedVenue(ctx, t, pool, "Astana venue", venueOpts{city: domain.CityAstana, isActive: true})
	seedPromo(ctx, t, pool, almatyVenue, "Скидка Алматы", domain.PromoPublished, open, close, domain.FeedApproved, 0)
	seedPromo(ctx, t, pool, astanaVenue, "Скидка Астаны", domain.PromoPublished, open, close, domain.FeedApproved, 0)
	seedPlatformPromo(ctx, t, pool, "Акция платформы", nil, open, close)
	seedPlatformEvent(ctx, t, pool, "Афиша платформы", nil, open, close)

	for city, venueCard := range map[domain.City]string{
		domain.CityAlmaty: "Скидка Алматы",
		domain.CityAstana: "Скидка Астаны",
	} {
		items, err := repo.ListCandidates(ctx, domain.FeedQuery{City: city, Now: feedNow, Limit: 100})
		if err != nil {
			t.Fatalf("ListCandidates(%s): %v", city, err)
		}
		if len(items) != 3 {
			t.Fatalf("%s: got %v, want the city's own card plus both platform cards", city, titlesOf(items))
		}
		mustContain(t, items, venueCard, "Акция платформы", "Афиша платформы")
	}
}

// The platform card carries no venue at all — nil id, empty name — and the
// venue flags it does not have cannot hide it. This is the row that would have
// blanked the home screen for everyone had the read model kept dereferencing a
// venue.
func TestListCandidates_PlatformCardCarriesNoVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	seedPlatformPromo(ctx, t, pool, "Акция платформы", nil, open, close)

	items, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %v, want the platform card", titlesOf(items))
	}
	it := items[0]
	if it.RestaurantID != nil {
		t.Fatalf("restaurant_id = %v, want nil", it.RestaurantID)
	}
	if it.RestaurantName != "" {
		t.Fatalf("restaurant_name = %q, want empty", it.RestaurantName)
	}
	if it.City != nil {
		t.Fatalf("city = %v, want nil (every city)", it.City)
	}
	// The read model must hand the domain rule a card it accepts, and the two
	// must agree — that agreement is what keeps an unapproved item off the
	// screen everywhere else in this file.
	if !domain.FeedEligible(it, domain.CityAlmaty, feedNow) {
		t.Fatal("the SQL returned a card domain.FeedEligible rejects")
	}
	if it.RestaurantRating != 0 || it.RestaurantReviewCount != 0 {
		t.Fatalf("a card with no venue must carry a neutral rating, got %.2f over %d",
			it.RestaurantRating, it.RestaurantReviewCount)
	}
}

// A platform card pinned to a city behaves like any city-scoped card.
func TestListCandidates_PlatformCardHonoursItsOwnCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	astana := domain.CityAstana
	seedPlatformPromo(ctx, t, pool, "Акция для Астаны", &astana, open, close)

	astanaItems, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAstana, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates(Astana): %v", err)
	}
	mustContain(t, astanaItems, "Акция для Астаны")

	almatyItems, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates(Almaty): %v", err)
	}
	if len(almatyItems) != 0 {
		t.Fatalf("Almaty got %v, want nothing", titlesOf(almatyItems))
	}
}

// An event's own city override now decides which main screen it reaches, the
// same way it already decides the Афиша. A venue-bound card WITHOUT an override
// is unaffected — it still follows its venue.
func TestListCandidates_EventCityOverrideMovesTheCard(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	almatyVenue := seedVenue(ctx, t, pool, "Almaty venue", activeVenue())
	stayer := seedEvent(ctx, t, pool, almatyVenue, "Дома", domain.EventPublished, open, close, domain.FeedApproved, 0)
	traveller := seedEvent(ctx, t, pool, almatyVenue, "Гастроли", domain.EventPublished, open, close, domain.FeedApproved, 0)
	if _, err := pool.Exec(ctx, `UPDATE events SET city = $2 WHERE id = $1`, traveller, string(domain.CityAstana)); err != nil {
		t.Fatalf("pin the event to Astana: %v", err)
	}
	_ = stayer

	almatyItems, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates(Almaty): %v", err)
	}
	if len(almatyItems) != 1 || almatyItems[0].Title != "Дома" {
		t.Fatalf("Almaty got %v, want only the event with no override", titlesOf(almatyItems))
	}

	astanaItems, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAstana, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates(Astana): %v", err)
	}
	if len(astanaItems) != 1 || astanaItems[0].Title != "Гастроли" {
		t.Fatalf("Astana got %v, want only the pinned event", titlesOf(astanaItems))
	}
	// It keeps its venue: an override moves the card, it does not orphan it.
	if astanaItems[0].RestaurantID == nil || *astanaItems[0].RestaurantID != almatyVenue {
		t.Fatalf("restaurant_id = %v, want the hosting venue", astanaItems[0].RestaurantID)
	}
}

// The venue-facing and platform-facing reads must not crash on a venue-less
// row either — the moderation queue is where the platform's own submission
// lands.
func TestFeedReads_PlatformItemInTheQueueAndByID(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	id := seedPlatformPromo(ctx, t, pool, "Акция платформы", nil, open, close)
	if _, err := pool.Exec(ctx,
		`UPDATE promos SET feed_status = 'pending_review', feed_submitted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("submit: %v", err)
	}

	queue, total, err := repo.ListByFeedStatus(ctx, domain.FeedPendingReview, 1, 20)
	if err != nil {
		t.Fatalf("ListByFeedStatus: %v", err)
	}
	if total != 1 || len(queue) != 1 || queue[0].RestaurantID != nil {
		t.Fatalf("queue = %+v, want the one venue-less submission", queue)
	}

	item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.RestaurantID != nil || item.RestaurantName != "" {
		t.Fatalf("item = %+v, want no venue", item)
	}
}
