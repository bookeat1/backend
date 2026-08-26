package feed

import (
	"context"
	"errors"
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

// --- auto-approval of the platform's own content (creation-time decision) ---

// seedUnmoderatedPlatformPromo inserts a PUBLISHED platform promo in the state
// a bare INSERT leaves it in: feed_status = 'not_submitted'. That is the state
// the owner's first platform promo actually sat in — correct row, invisible
// card — and it is what ApprovePlatformItem exists to resolve at creation.
func seedUnmoderatedPlatformPromo(ctx context.Context, t *testing.T, pool sqltx.Querier,
	title string, startsAt, endsAt time.Time) uuid.UUID {
	t.Helper()
	p := &domain.Promo{Title: title, Status: domain.PromoPublished, StartsAt: startsAt, EndsAt: endsAt}
	if err := promorepo.New(pool).Create(ctx, p); err != nil {
		t.Fatalf("seed unmoderated platform promo: %v", err)
	}
	return p.ID
}

func feedStatusOf(ctx context.Context, t *testing.T, pool sqltx.Querier, table string, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT feed_status FROM `+table+` WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read %s.feed_status: %v", table, err)
	}
	return status
}

// The whole point of the change: the platform's own promo reaches the home
// screen without a moderation round trip, and the approval is WRITTEN DOWN —
// who decided and when — instead of being inferred while reading the feed.
func TestApprovePlatformItem_PutsThePlatformsOwnCardOnTheHomeScreen(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	id := seedUnmoderatedPlatformPromo(ctx, t, pool, "Акция платформы", open, close)

	before, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("an unapproved card must not be on the main screen, got %v", titlesOf(before))
	}

	reviewer := seedUser(ctx, t, pool, "superadmin")
	at := feedNow.Add(-30 * time.Minute)
	if err := repo.ApprovePlatformItem(ctx, domain.FeedItemPromo, id, reviewer, at); err != nil {
		t.Fatalf("ApprovePlatformItem: %v", err)
	}

	item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Placement.Status != domain.FeedApproved {
		t.Fatalf("feed_status = %q, want approved", item.Placement.Status)
	}
	if item.Placement.ReviewedBy == nil || *item.Placement.ReviewedBy != reviewer {
		t.Fatalf("reviewed_by = %v, want the superadmin who created it (%s)", item.Placement.ReviewedBy, reviewer)
	}
	if item.Placement.ReviewedAt == nil || !item.Placement.ReviewedAt.Equal(at) {
		t.Fatalf("reviewed_at = %v, want %v", item.Placement.ReviewedAt, at)
	}
	// An approved row that was never submitted would read like a bug in the
	// audit trail: the platform submitted and decided in the same act.
	if item.Placement.SubmittedAt == nil || !item.Placement.SubmittedAt.Equal(at) {
		t.Fatalf("submitted_at = %v, want %v", item.Placement.SubmittedAt, at)
	}

	after, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	mustContain(t, after, "Акция платформы")
}

// The guard that keeps venue moderation intact lives in the WHERE clause, so a
// caller cannot use this write to approve a VENUE's promo without a moderator.
func TestApprovePlatformItem_RefusesVenueContent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	venue := seedVenue(ctx, t, pool, "Venue", activeVenue())
	id := seedPromo(ctx, t, pool, venue, "Скидка заведения", domain.PromoPublished, open, close, domain.FeedNotSubmitted, 0)
	reviewer := seedUser(ctx, t, pool, "superadmin")

	err := repo.ApprovePlatformItem(ctx, domain.FeedItemPromo, id, reviewer, feedNow)
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus for a venue-owned promo", err)
	}
	if got := feedStatusOf(ctx, t, pool, "promos", id); got != string(domain.FeedNotSubmitted) {
		t.Fatalf("feed_status = %q, want the venue promo left untouched", got)
	}
	items, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("a venue promo still needs approval, got %v on the main screen", titlesOf(items))
	}
}

// A repeated call (a retried create, a duplicated request) must not re-stamp a
// reviewer over a decision that already exists, and an absent id is a 404, not
// a silent success.
func TestApprovePlatformItem_RefusesAnAlreadyDecidedRowAndAnAbsentOne(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	id := seedUnmoderatedPlatformPromo(ctx, t, pool, "Акция платформы", open, close)
	first := seedUser(ctx, t, pool, "first superadmin")
	second := seedUser(ctx, t, pool, "second superadmin")

	if err := repo.ApprovePlatformItem(ctx, domain.FeedItemPromo, id, first, feedNow); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	err := repo.ApprovePlatformItem(ctx, domain.FeedItemPromo, id, second, feedNow.Add(time.Hour))
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus for an already decided row", err)
	}
	item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Placement.ReviewedBy == nil || *item.Placement.ReviewedBy != first {
		t.Fatalf("reviewed_by = %v, want the first decider %s", item.Placement.ReviewedBy, first)
	}

	if err := repo.ApprovePlatformItem(ctx, domain.FeedItemPromo, uuid.New(), first, feedNow); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an absent id", err)
	}
}

// Editing content demotes a VENUE card (this is the rule that stops "get an
// innocuous promo approved, then edit the title") and leaves a PLATFORM card
// alone — there is no second party to re-review it, so demoting it would hide
// the platform's own content behind a review nobody can perform.
func TestDemoteAfterContentEdit_ExemptsPlatformContent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	repo := New(pool)

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	venue := seedVenue(ctx, t, pool, "Venue", activeVenue())
	venuePromo := seedPromo(ctx, t, pool, venue, "Скидка заведения", domain.PromoPublished, open, close, domain.FeedApproved, 0)
	platformPromo := seedPlatformPromo(ctx, t, pool, "Акция платформы", nil, open, close)

	if err := repo.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, venuePromo); err != nil {
		t.Fatalf("demote venue promo: %v", err)
	}
	if err := repo.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, platformPromo); err != nil {
		t.Fatalf("demote platform promo: %v", err)
	}

	if got := feedStatusOf(ctx, t, pool, "promos", venuePromo); got != string(domain.FeedPendingReview) {
		t.Fatalf("venue promo feed_status = %q, want pending_review", got)
	}
	if got := feedStatusOf(ctx, t, pool, "promos", platformPromo); got != string(domain.FeedApproved) {
		t.Fatalf("platform promo feed_status = %q, want approved (exempt)", got)
	}
}
