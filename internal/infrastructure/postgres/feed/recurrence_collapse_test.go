package feed

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	usecasefeed "backend-core/internal/usecase/feed"
)

// The main screen collapses a recurring series into ONE card, exactly like the
// public Афиша does (event.ListPublicUpcoming). The rule lives in SQL — a
// window function inside the event branch of the union — so it is tested
// against real Postgres; a fake repository would only re-run Go code that has
// no say in it.
//
// The incident behind these tests: six approved daily rules turned the feed
// into ~98 cards, «Живая музыка в ресторане INZHU» repeating over and over.

// collapseFeedTables adds the recurrence tables to the set feedTables owns.
// event_recurrences is a PARENT of events, so truncating events alone would
// leave rules behind and let one case seed rows another case sees.
var collapseFeedTables = []string{
	"event_recurrence_skips", "event_images", "promo_images",
	"events", "event_recurrences", "promos",
	"reviews", "user_cuisine_preferences", "restaurant_categories", "restaurants", "users",
}

// seedFeedRule inserts a recurrence rule directly. This package tests the feed
// read model, and going through the recurrence repository would only add a
// dependency on behaviour that has no say here. The template values are the
// minimum the CHECKs of migration 0074 accept.
func seedFeedRule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_recurrences (id, restaurant_id, title, frequency, weekdays,
			start_minutes, duration_minutes, starts_on, is_active)
		 VALUES ($1, $2, $3, 'daily', '{}', 1140, 180, current_date, true)`,
		id, rid, title); err != nil {
		t.Fatalf("seed recurrence rule: %v", err)
	}
	return id
}

// seedFeedOccurrence creates a published, feed-approved event that belongs to a
// rule. events.recurrence_id is written by the generator only, never by
// Create(), so the link is set right after.
func seedFeedOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rid, ruleID uuid.UUID,
	title string, startsAt, endsAt time.Time) uuid.UUID {
	t.Helper()
	id := seedEvent(ctx, t, pool, rid, title, domain.EventPublished, startsAt, endsAt, domain.FeedApproved, 0)
	if _, err := pool.Exec(ctx, `UPDATE events SET recurrence_id = $2 WHERE id = $1`, id, ruleID); err != nil {
		t.Fatalf("link occurrence to rule: %v", err)
	}
	return id
}

// seedDailySeries generates `count` daily occurrences starting `firstStartsIn`
// from feedNow and returns their ids in chronological order.
func seedDailySeries(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rid, ruleID uuid.UUID,
	title string, firstStartsIn time.Duration, count int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, count)
	for i := 0; i < count; i++ {
		startsAt := feedNow.Add(firstStartsIn + time.Duration(i)*24*time.Hour)
		ids = append(ids, seedFeedOccurrence(ctx, t, pool, rid, ruleID, title, startsAt, startsAt.Add(3*time.Hour)))
	}
	return ids
}

func idsByTitle(items []domain.FeedItem, title string) []uuid.UUID {
	var out []uuid.UUID
	for _, it := range items {
		if it.Title == title {
			out = append(out, it.ID)
		}
	}
	return out
}

func hasID(items []domain.FeedItem, id uuid.UUID) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// TestListCandidates_RecurringSeriesCollapsesToNearestUpcoming is the incident
// test. A daily rule with many approved occurrences must contribute exactly ONE
// card, and it must be the nearest one still ahead — not the first ever
// generated, and never a date that already passed. One-off events and promos
// must be untouched: they all share the NULL recurrence partition, so a plain
// row_number without the explicit IS NULL branch would delete all but one of
// them, and the assertions below are written to catch exactly that.
func TestListCandidates_RecurringSeriesCollapsesToNearestUpcoming(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseFeedTables...)
	ctx := context.Background()

	// The city is set EXPLICITLY: a venue seeded with some other spelling would
	// make an "and nothing else is in the feed" assertion pass for the wrong
	// reason.
	venue := seedVenue(ctx, t, pool, "INZHU", venueOpts{city: domain.CityAlmaty, isActive: true})
	other := seedVenue(ctx, t, pool, "Second venue", venueOpts{city: domain.CityAlmaty, isActive: true})

	rule := seedFeedRule(ctx, t, pool, venue, "Живая музыка")
	// Two dates already over: they must not be able to win the one slot.
	seedFeedOccurrence(ctx, t, pool, venue, rule, "Живая музыка",
		feedNow.Add(-48*time.Hour), feedNow.Add(-45*time.Hour))
	seedFeedOccurrence(ctx, t, pool, venue, rule, "Живая музыка",
		feedNow.Add(-24*time.Hour), feedNow.Add(-21*time.Hour))
	future := seedDailySeries(ctx, t, pool, venue, rule, "Живая музыка", 6*time.Hour, 20)

	// A second, independent rule at another venue: partitions must not merge.
	otherRule := seedFeedRule(ctx, t, pool, other, "Караоке-битва")
	otherFuture := seedDailySeries(ctx, t, pool, other, otherRule, "Караоке-битва", 10*time.Hour, 8)

	// Two one-off events (both recurrence_id IS NULL) and one promo.
	oneOffA := seedEvent(ctx, t, pool, venue, "Дегустация", domain.EventPublished,
		feedNow.Add(30*time.Hour), feedNow.Add(34*time.Hour), domain.FeedApproved, 0)
	oneOffB := seedEvent(ctx, t, pool, other, "Джаз-вечер", domain.EventPublished,
		feedNow.Add(50*time.Hour), feedNow.Add(54*time.Hour), domain.FeedApproved, 0)
	promo := seedPromo(ctx, t, pool, venue, "Скидка 20%", domain.PromoPublished,
		feedNow.Add(-time.Hour), feedNow.Add(72*time.Hour), domain.FeedApproved, 0)

	items, err := New(pool).ListCandidates(ctx, domain.FeedQuery{
		City: domain.CityAlmaty, Now: feedNow, Limit: 500,
	})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}

	// Positive assertions first: the four cards that MUST be there.
	if got := idsByTitle(items, "Живая музыка"); len(got) != 1 || got[0] != future[0] {
		t.Fatalf("recurring series: want exactly one card %s (the nearest upcoming), got %v", future[0], got)
	}
	if got := idsByTitle(items, "Караоке-битва"); len(got) != 1 || got[0] != otherFuture[0] {
		t.Fatalf("second series: want exactly one card %s, got %v", otherFuture[0], got)
	}
	for _, id := range []uuid.UUID{oneOffA, oneOffB, promo} {
		if !hasID(items, id) {
			t.Fatalf("non-recurring item %s disappeared from the feed", id)
		}
	}
	// ...and then the total: 33 seeded rows, 5 cards.
	if len(items) != 5 {
		t.Fatalf("want 5 feed cards after the collapse, got %d: %v", len(items), titles(items))
	}
}

// TestListCandidates_NextOccurrenceTakesOverWhenTheNearestPasses proves the
// collapse is a live rule, not a snapshot: the window function runs over the
// already-filtered set, so once today's date ends, tomorrow's is the nearest
// upcoming one and takes the card.
func TestListCandidates_NextOccurrenceTakesOverWhenTheNearestPasses(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseFeedTables...)
	ctx := context.Background()

	venue := seedVenue(ctx, t, pool, "INZHU", venueOpts{city: domain.CityAlmaty, isActive: true})
	rule := seedFeedRule(ctx, t, pool, venue, "Живая музыка")
	series := seedDailySeries(ctx, t, pool, venue, rule, "Живая музыка", 6*time.Hour, 5)

	repo := New(pool)
	// The first occurrence runs feedNow+6h .. feedNow+9h; step just past its end.
	afterFirst := feedNow.Add(6*time.Hour + 3*time.Hour + time.Minute)

	items, err := repo.ListCandidates(ctx, domain.FeedQuery{
		City: domain.CityAlmaty, Now: afterFirst, Limit: 500,
	})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if got := idsByTitle(items, "Живая музыка"); len(got) != 1 || got[0] != series[1] {
		t.Fatalf("after the first date ended: want exactly one card %s (the second date), got %v", series[1], got)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 card, got %d: %v", len(items), titles(items))
	}
}

// TestMain_TotalMatchesTheCollapsedSet closes the loop the guest actually
// feels: usecase/feed reports Total as the size of the candidate set, so a
// collapse that happened anywhere later than the repository would leave the
// client paging into emptiness. Times here are anchored to time.Now() because
// the facade's clock is not injectable from outside its package.
func TestMain_TotalMatchesTheCollapsedSet(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseFeedTables...)
	ctx := context.Background()

	now := time.Now().UTC()
	venue := seedVenue(ctx, t, pool, "INZHU", venueOpts{city: domain.CityAlmaty, isActive: true})
	rule := seedFeedRule(ctx, t, pool, venue, "Живая музыка")
	for i := 0; i < 30; i++ {
		startsAt := now.Add(time.Duration(i)*24*time.Hour + 6*time.Hour)
		seedFeedOccurrence(ctx, t, pool, venue, rule, "Живая музыка", startsAt, startsAt.Add(3*time.Hour))
	}
	seedEvent(ctx, t, pool, venue, "Дегустация", domain.EventPublished,
		now.Add(30*time.Hour), now.Add(34*time.Hour), domain.FeedApproved, 0)
	seedPromo(ctx, t, pool, venue, "Скидка 20%", domain.PromoPublished,
		now.Add(-time.Hour), now.Add(72*time.Hour), domain.FeedApproved, 0)

	res, err := usecasefeed.NewFacade(New(pool), nil).Main(ctx, usecasefeed.MainInput{
		City: domain.CityAlmaty, Page: 1, PerPage: 20,
	})
	if err != nil {
		t.Fatalf("feed main: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("want Total 3 (collapsed series + one-off event + promo), got %d", res.Total)
	}
	if len(res.Items) != res.Total {
		t.Fatalf("Total %d disagrees with the %d items actually returned", res.Total, len(res.Items))
	}
	var recurring int
	for _, it := range res.Items {
		if it.Item.Title == "Живая музыка" {
			recurring++
		}
	}
	if recurring != 1 {
		t.Fatalf("want exactly 1 card for the series on the page, got %d", recurring)
	}
}

func titles(items []domain.FeedItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}
