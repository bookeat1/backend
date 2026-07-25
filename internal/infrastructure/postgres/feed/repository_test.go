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
	"backend-core/internal/infrastructure/postgres/restaurant"
	reviewrepo "backend-core/internal/infrastructure/postgres/review"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

// feedNow is the frozen instant every window in these tests is measured
// against; never time.Now(), so a boundary case cannot depend on the clock.
var feedNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// The tables these tests own. Truncated before each case so one leftover row
// cannot make another test pass.
var feedTables = []string{"promos", "events", "reviews", "user_cuisine_preferences",
	"restaurant_categories", "restaurants", "users"}

func seedUser(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	u := &domain.User{ID: uuid.New(), FullName: name, Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := userrepo.New(pool).Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func seedCategory(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_categories (id, name) VALUES ($1, $2)`, id, name); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	return id
}

type venueOpts struct {
	city           domain.City
	categoryID     *uuid.UUID
	isActive       bool
	hiddenFromHome bool
}

func seedVenue(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, o venueOpts) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{
		ID: uuid.New(), Name: name, City: o.city, PriceCategory: domain.PriceMid,
		CategoryID: o.categoryID, IsActive: o.isActive, HiddenFromHome: o.hiddenFromHome,
	}
	if err := restaurant.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	// Create() does not carry hidden_from_home in every code path; set it
	// explicitly so the test states the flag it is actually asserting on.
	if _, err := pool.Exec(ctx,
		`UPDATE restaurants SET hidden_from_home = $2, is_active = $3 WHERE id = $1`,
		r.ID, o.hiddenFromHome, o.isActive); err != nil {
		t.Fatalf("set venue flags: %v", err)
	}
	return r.ID
}

func activeVenue() venueOpts {
	return venueOpts{city: domain.CityAlmaty, isActive: true}
}

// seedPromo inserts a promo and puts it in the given feed status directly, so a
// case can start from any moderation state without walking the whole flow.
func seedPromo(ctx context.Context, t *testing.T, pool sqltx.Querier, rid uuid.UUID,
	title string, status domain.PromoStatus, startsAt, endsAt time.Time,
	feedStatus domain.FeedStatus, weight int) uuid.UUID {
	t.Helper()
	p := &domain.Promo{
		RestaurantID: rid, Title: title, Status: status,
		StartsAt: startsAt, EndsAt: endsAt,
	}
	if err := promorepo.New(pool).Create(ctx, p); err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE promos SET feed_status = $2, feed_placement_weight = $3 WHERE id = $1`,
		p.ID, string(feedStatus), weight); err != nil {
		t.Fatalf("set promo feed state: %v", err)
	}
	return p.ID
}

func seedEvent(ctx context.Context, t *testing.T, pool sqltx.Querier, rid uuid.UUID,
	title string, status domain.EventStatus, startsAt, endsAt time.Time,
	feedStatus domain.FeedStatus, weight int) uuid.UUID {
	t.Helper()
	e := &domain.Event{
		RestaurantID: rid, Title: title, Status: status,
		StartsAt: startsAt, EndsAt: endsAt,
		TicketsRefundable:         domain.DefaultTicketRefundPolicy.Refundable,
		TicketRefundCutoffMinutes: domain.DefaultTicketRefundPolicy.CutoffMinutes,
	}
	if err := eventrepo.New(pool).Create(ctx, e); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE events SET feed_status = $2, feed_placement_weight = $3 WHERE id = $1`,
		e.ID, string(feedStatus), weight); err != nil {
		t.Fatalf("set event feed state: %v", err)
	}
	return e.ID
}

// TestListCandidates_SQLAgreesWithDomainFeedEligible is the test that matters
// most in this package: the guest read filters in SQL for speed, while
// domain.FeedEligible is the WRITTEN rule the usecase tests are built on. If
// the two ever disagree, an unapproved item reaches the main screen. Here every
// row is seeded twice — once as a database row, once as the equivalent
// domain.FeedItem — and the two verdicts are compared.
func TestListCandidates_SQLAgreesWithDomainFeedEligible(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()

	almaty := seedVenue(ctx, t, pool, "Almaty venue", activeVenue())
	astana := seedVenue(ctx, t, pool, "Astana venue", venueOpts{city: domain.CityAstana, isActive: true})
	inactive := seedVenue(ctx, t, pool, "Closed venue", venueOpts{city: domain.CityAlmaty})
	hidden := seedVenue(ctx, t, pool, "Hidden venue", venueOpts{city: domain.CityAlmaty, isActive: true, hiddenFromHome: true})

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)

	cases := []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"published+approved+in window", seedPromo(ctx, t, pool, almaty, "eligible", domain.PromoPublished, open, close, domain.FeedApproved, 0), true},
		{"never submitted", seedPromo(ctx, t, pool, almaty, "not submitted", domain.PromoPublished, open, close, domain.FeedNotSubmitted, 0), false},
		{"awaiting review", seedPromo(ctx, t, pool, almaty, "pending", domain.PromoPublished, open, close, domain.FeedPendingReview, 0), false},
		{"rejected", seedPromo(ctx, t, pool, almaty, "rejected", domain.PromoPublished, open, close, domain.FeedRejected, 0), false},
		{"approved but a draft", seedPromo(ctx, t, pool, almaty, "draft", domain.PromoDraft, open, close, domain.FeedApproved, 0), false},
		{"approved but hidden by the venue", seedPromo(ctx, t, pool, almaty, "hidden", domain.PromoHidden, open, close, domain.FeedApproved, 0), false},
		{"expired", seedPromo(ctx, t, pool, almaty, "expired", domain.PromoPublished, feedNow.Add(-48*time.Hour), feedNow.Add(-time.Minute), domain.FeedApproved, 0), false},
		{"window not open yet", seedPromo(ctx, t, pool, almaty, "future", domain.PromoPublished, feedNow.Add(time.Hour), close, domain.FeedApproved, 0), false},
		{"another city", seedPromo(ctx, t, pool, astana, "elsewhere", domain.PromoPublished, open, close, domain.FeedApproved, 0), false},
		{"deactivated venue", seedPromo(ctx, t, pool, inactive, "closed venue", domain.PromoPublished, open, close, domain.FeedApproved, 0), false},
		{"venue hidden from home", seedPromo(ctx, t, pool, hidden, "hidden venue", domain.PromoPublished, open, close, domain.FeedApproved, 0), false},
		// The documented asymmetry: an event is promoted BEFORE it starts.
		{"upcoming event", seedEvent(ctx, t, pool, almaty, "upcoming", domain.EventPublished, feedNow.Add(72*time.Hour), feedNow.Add(96*time.Hour), domain.FeedApproved, 0), true},
		{"ended event", seedEvent(ctx, t, pool, almaty, "over", domain.EventPublished, feedNow.Add(-96*time.Hour), feedNow.Add(-time.Minute), domain.FeedApproved, 0), false},
	}

	repo := New(pool)
	got, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	inFeed := map[uuid.UUID]bool{}
	for _, it := range got {
		inFeed[it.ID] = true
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if inFeed[tc.id] != tc.want {
				t.Fatalf("SQL says eligible=%v, want %v", inFeed[tc.id], tc.want)
			}
			// And the domain predicate must reach the same verdict for the same
			// row, read back through GetItem.
			kind := domain.FeedItemPromo
			if _, err := repo.GetItem(ctx, kind, tc.id); errors.Is(err, domain.ErrNotFound) {
				kind = domain.FeedItemEvent
			}
			item, err := repo.GetItem(ctx, kind, tc.id)
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if domain.FeedEligible(*item, domain.CityAlmaty, feedNow) != tc.want {
				t.Fatalf("domain.FeedEligible disagrees with the SQL for %+v", item)
			}
		})
	}
}

func TestListCandidates_CarriesRatingAndPreferences(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()

	italian := seedCategory(ctx, t, pool, "Italian")
	sushi := seedCategory(ctx, t, pool, "Sushi")
	matching := seedVenue(ctx, t, pool, "Trattoria", venueOpts{city: domain.CityAlmaty, isActive: true, categoryID: &italian})
	other := seedVenue(ctx, t, pool, "Sushi bar", venueOpts{city: domain.CityAlmaty, isActive: true, categoryID: &sushi})

	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)
	seedPromo(ctx, t, pool, matching, "pasta week", domain.PromoPublished, open, close, domain.FeedApproved, 0)
	seedPromo(ctx, t, pool, other, "roll week", domain.PromoPublished, open, close, domain.FeedApproved, 0)

	// Two published reviews for the Italian venue, one hidden — the hidden one
	// must not move the average.
	reviews := reviewrepo.New(pool)
	for _, rating := range []int{5, 4} {
		uid := seedUser(ctx, t, pool, "Guest")
		rv := &domain.Review{RestaurantID: matching, UserID: uid, Rating: rating, Status: domain.ReviewPublished}
		if err := reviews.Upsert(ctx, rv); err != nil {
			t.Fatalf("seed review: %v", err)
		}
	}
	hiddenReviewer := seedUser(ctx, t, pool, "Hidden guest")
	hiddenReview := &domain.Review{RestaurantID: matching, UserID: hiddenReviewer, Rating: 1, Status: domain.ReviewPublished}
	if err := reviews.Upsert(ctx, hiddenReview); err != nil {
		t.Fatalf("seed hidden review: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reviews SET status = 'hidden' WHERE id = $1`, hiddenReview.ID); err != nil {
		t.Fatalf("hide review: %v", err)
	}

	guest := seedUser(ctx, t, pool, "Foodie")
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_cuisine_preferences (user_id, category_id) VALUES ($1, $2)`, guest, italian); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	repo := New(pool)

	signedIn, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, UserID: &guest, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates (signed in): %v", err)
	}
	if len(signedIn) != 2 {
		t.Fatalf("expected both promos, got %d", len(signedIn))
	}
	for _, it := range signedIn {
		if !it.HasCuisinePreferences {
			t.Fatal("a guest with preferences must be reported as having them")
		}
		wantMatch := it.RestaurantID == matching
		if it.MatchesCuisinePreference != wantMatch {
			t.Fatalf("venue %s: match=%v, want %v", it.RestaurantName, it.MatchesCuisinePreference, wantMatch)
		}
		if it.RestaurantID == matching {
			if it.RestaurantReviewCount != 2 || it.RestaurantRating != 4.5 {
				t.Fatalf("rating aggregate must count published reviews only, got %.2f over %d",
					it.RestaurantRating, it.RestaurantReviewCount)
			}
		}
	}

	// The "preferences absent" path: an anonymous guest.
	anon, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 100})
	if err != nil {
		t.Fatalf("ListCandidates (anonymous): %v", err)
	}
	for _, it := range anon {
		if it.HasCuisinePreferences || it.MatchesCuisinePreference {
			t.Fatalf("an anonymous guest must carry no preference signal, got %+v", it)
		}
	}
}

func TestListCandidates_PreOrderAndLimitAreDeterministic(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	rid := seedVenue(ctx, t, pool, "Venue", activeVenue())
	open := feedNow.Add(-time.Hour)

	seedPromo(ctx, t, pool, rid, "low", domain.PromoPublished, open, feedNow.Add(10*time.Hour), domain.FeedApproved, 10)
	seedPromo(ctx, t, pool, rid, "high", domain.PromoPublished, open, feedNow.Add(200*time.Hour), domain.FeedApproved, 90)
	seedPromo(ctx, t, pool, rid, "mid", domain.PromoPublished, open, feedNow.Add(100*time.Hour), domain.FeedApproved, 50)

	repo := New(pool)
	// The cap must keep the highest-weight rows, and repeating the call must
	// return the identical truncation.
	var first []uuid.UUID
	for i := 0; i < 3; i++ {
		got, err := repo.ListCandidates(ctx, domain.FeedQuery{City: domain.CityAlmaty, Now: feedNow, Limit: 2})
		if err != nil {
			t.Fatalf("ListCandidates: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("the limit must be honoured, got %d", len(got))
		}
		ids := []uuid.UUID{got[0].ID, got[1].ID}
		if i == 0 {
			first = ids
			if got[0].Title != "high" || got[1].Title != "mid" {
				t.Fatalf("the pre-order must keep the highest weights, got %s then %s", got[0].Title, got[1].Title)
			}
			continue
		}
		if ids[0] != first[0] || ids[1] != first[1] {
			t.Fatalf("the truncation must be reproducible: %v vs %v", ids, first)
		}
	}
}

func TestTransitionFeedStatus_CASAndSentinels(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	rid := seedVenue(ctx, t, pool, "Venue", activeVenue())
	reviewer := seedUser(ctx, t, pool, "Superadmin")
	id := seedPromo(ctx, t, pool, rid, "promo", domain.PromoPublished,
		feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour), domain.FeedNotSubmitted, 30)
	repo := New(pool)

	submittedAt := feedNow
	if err := repo.TransitionFeedStatus(ctx, domain.FeedItemPromo, id,
		[]domain.FeedStatus{domain.FeedNotSubmitted, domain.FeedRejected},
		domain.FeedPlacementUpdate{Status: domain.FeedPendingReview, SubmittedAt: &submittedAt}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Placement.Status != domain.FeedPendingReview {
		t.Fatalf("status = %s, want pending_review", item.Placement.Status)
	}
	// A moderation write with no weight must leave the sold weight alone.
	if item.Placement.PlacementWeight != 30 {
		t.Fatalf("the placement weight must survive a transition, got %d", item.Placement.PlacementWeight)
	}

	// Repeating the same transition now loses the CAS.
	if err := repo.TransitionFeedStatus(ctx, domain.FeedItemPromo, id,
		[]domain.FeedStatus{domain.FeedNotSubmitted},
		domain.FeedPlacementUpdate{Status: domain.FeedPendingReview}); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("a lost CAS must be ErrInvalidStatus, got %v", err)
	}
	// An absent id is ErrNotFound, not ErrInvalidStatus.
	if err := repo.TransitionFeedStatus(ctx, domain.FeedItemPromo, uuid.New(),
		[]domain.FeedStatus{domain.FeedNotSubmitted},
		domain.FeedPlacementUpdate{Status: domain.FeedPendingReview}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an absent id must be ErrNotFound, got %v", err)
	}

	// Approve, this time pricing the placement in the same call.
	weight := 70
	reviewedAt := feedNow.Add(time.Minute)
	if err := repo.TransitionFeedStatus(ctx, domain.FeedItemPromo, id,
		[]domain.FeedStatus{domain.FeedPendingReview},
		domain.FeedPlacementUpdate{
			Status: domain.FeedApproved, SubmittedAt: &submittedAt,
			ReviewedBy: &reviewer, ReviewedAt: &reviewedAt, PlacementWeight: &weight,
		}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	item, err = repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Placement.Status != domain.FeedApproved || item.Placement.PlacementWeight != 70 {
		t.Fatalf("approval did not stick: %+v", item.Placement)
	}
	if item.Placement.ReviewedBy == nil || *item.Placement.ReviewedBy != reviewer {
		t.Fatalf("the reviewer must be recorded, got %v", item.Placement.ReviewedBy)
	}
}

// The moderation hole: get something bland approved, then rewrite it.
func TestDemoteAfterContentEdit(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	rid := seedVenue(ctx, t, pool, "Venue", activeVenue())
	repo := New(pool)
	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)

	for _, tc := range []struct {
		from, want domain.FeedStatus
	}{
		{domain.FeedApproved, domain.FeedPendingReview},
		{domain.FeedRejected, domain.FeedPendingReview},
		{domain.FeedPendingReview, domain.FeedPendingReview},
		{domain.FeedNotSubmitted, domain.FeedNotSubmitted},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			id := seedPromo(ctx, t, pool, rid, "promo", domain.PromoPublished, open, close, tc.from, 40)
			if err := repo.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, id); err != nil {
				t.Fatalf("demote: %v", err)
			}
			item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if item.Placement.Status != tc.want {
				t.Fatalf("status = %s, want %s", item.Placement.Status, tc.want)
			}
			// The SQL must agree with the written rule.
			if domain.FeedStatusAfterContentEdit(tc.from) != item.Placement.Status {
				t.Fatalf("SQL disagrees with domain.FeedStatusAfterContentEdit for %s", tc.from)
			}
			// Editing never costs the venue its paid slot.
			if item.Placement.PlacementWeight != 40 {
				t.Fatalf("an edit must not touch the placement weight, got %d", item.Placement.PlacementWeight)
			}
		})
	}

	// An id that no longer exists is not an error — the caller is mid-edit.
	if err := repo.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, uuid.New()); err != nil {
		t.Fatalf("demoting an absent id must be a no-op, got %v", err)
	}
}

func TestListByRestaurantAndQueue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	mine := seedVenue(ctx, t, pool, "Mine", activeVenue())
	theirs := seedVenue(ctx, t, pool, "Theirs", activeVenue())
	open, close := feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour)

	seedPromo(ctx, t, pool, mine, "my promo", domain.PromoPublished, open, close, domain.FeedPendingReview, 0)
	seedEvent(ctx, t, pool, mine, "my event", domain.EventPublished, open, close, domain.FeedNotSubmitted, 0)
	seedPromo(ctx, t, pool, theirs, "their promo", domain.PromoPublished, open, close, domain.FeedPendingReview, 0)

	repo := New(pool)
	items, total, err := repo.ListByRestaurant(ctx, mine, 1, 20)
	if err != nil {
		t.Fatalf("ListByRestaurant: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("a venue must see exactly its own promos and events, got %d/%d", len(items), total)
	}
	for _, it := range items {
		if it.RestaurantID != mine {
			t.Fatalf("another venue's item leaked into the list: %s", it.Title)
		}
	}

	queue, qtotal, err := repo.ListByFeedStatus(ctx, domain.FeedPendingReview, 1, 20)
	if err != nil {
		t.Fatalf("ListByFeedStatus: %v", err)
	}
	if qtotal != 2 || len(queue) != 2 {
		t.Fatalf("the platform queue spans every venue, got %d/%d", len(queue), qtotal)
	}
	for _, it := range queue {
		if it.Placement.Status != domain.FeedPendingReview {
			t.Fatalf("only pending items belong in the queue, got %s", it.Placement.Status)
		}
	}
}

func TestSetPlacementWeight(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, feedTables...)
	ctx := context.Background()
	rid := seedVenue(ctx, t, pool, "Venue", activeVenue())
	id := seedPromo(ctx, t, pool, rid, "promo", domain.PromoPublished,
		feedNow.Add(-time.Hour), feedNow.Add(48*time.Hour), domain.FeedApproved, 0)
	repo := New(pool)

	if err := repo.SetPlacementWeight(ctx, domain.FeedItemPromo, id, 55); err != nil {
		t.Fatalf("set weight: %v", err)
	}
	item, err := repo.GetItem(ctx, domain.FeedItemPromo, id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Placement.PlacementWeight != 55 {
		t.Fatalf("weight = %d, want 55", item.Placement.PlacementWeight)
	}
	// The dial must not disturb the moderation decision.
	if item.Placement.Status != domain.FeedApproved {
		t.Fatalf("pricing must not change the moderation status, got %s", item.Placement.Status)
	}
	if err := repo.SetPlacementWeight(ctx, domain.FeedItemPromo, uuid.New(), 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an absent id must be ErrNotFound, got %v", err)
	}
}
