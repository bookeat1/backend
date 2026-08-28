package eventrecurrence

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	feedrepo "backend-core/internal/infrastructure/postgres/feed"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// What this file proves is a SQL fact, not a Go one: an occurrence generated
// from an approved rule really is read back by the home feed's own query
// (feed.Repository.ListCandidates, which is what the app's main screen calls),
// and one generated from a rule nobody approved really is not. A fake feed
// repository would only re-state the rule we are trying to verify.

// feedHarness is the recurrence harness plus everything the moderation flow
// needs: the admin CRUD facade, a venue manager, a platform superadmin.
type feedHarness struct {
	*harness
	facade    Facade
	feed      *feedrepo.Repository
	now       time.Time
	venue     Actor
	superuser Actor
}

func newFeedHarness(t *testing.T, now time.Time, window time.Duration) *feedHarness {
	t.Helper()
	h := newHarness(t, "UTC", window, now)
	// newHarness truncated only the recurrence tables; the feed read model also
	// unions promos, and the reviewer stamp needs a real users row (FK).
	testdb.Truncate(t, h.pool, "promo_images", "promos", "users")
	// The recurrence harness seeds the venue with the raw string 'almaty', which
	// is NOT domain.CityAlmaty ("Алматы") — the feed is city-scoped, so a test
	// that queried the wrong city would pass for the wrong reason.
	if _, err := h.pool.Exec(h.ctx, `UPDATE restaurants SET city = $2 WHERE id = $1`,
		h.restaurantID, string(domain.CityAlmaty)); err != nil {
		t.Fatalf("set venue city: %v", err)
	}

	venueUser, superUser := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{venueUser, superUser} {
		if _, err := h.pool.Exec(h.ctx,
			`INSERT INTO users (id, full_name, role) VALUES ($1, 'staff', 'user')`, id); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	return &feedHarness{
		harness:   h,
		facade:    NewFacade(h.repo, permsWith(venueUser, h.restaurantID, domain.StaffRoleOwner), WithClock(func() time.Time { return now })),
		feed:      feedrepo.New(h.pool),
		now:       now,
		venue:     Actor{UserID: venueUser, Role: domain.RoleRestaurant},
		superuser: Actor{UserID: superUser, Role: domain.RoleAdmin},
	}
}

// dailyRule is «Живая музыка в ресторане INZHU»: every day at 19:00, published
// occurrences — the exact rule that is live in production.
func (h *feedHarness) dailyRule(t *testing.T, title string) *domain.EventRecurrence {
	t.Helper()
	rec := &domain.EventRecurrence{
		RestaurantID:     h.restaurantID,
		Title:            title,
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceDaily,
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         domain.CalendarDate{Year: h.now.Year(), Month: h.now.Month(), Day: h.now.Day()},
		IsActive:         true,
	}
	if err := h.repo.Create(h.ctx, rec); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return rec
}

// feedEventIDs runs the guest main-screen query and returns the event cards it
// yields.
func (h *feedHarness) feedEventIDs(t *testing.T) map[uuid.UUID]bool {
	t.Helper()
	items, err := h.feed.ListCandidates(h.ctx, domain.FeedQuery{
		City:  domain.CityAlmaty,
		Now:   h.now,
		Limit: 500,
	})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, it := range items {
		if it.Kind == domain.FeedItemEvent {
			out[it.ID] = true
		}
	}
	return out
}

func (h *feedHarness) occurrenceIDs(t *testing.T, ruleID uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := h.pool.Query(h.ctx, `SELECT id FROM events WHERE recurrence_id = $1 ORDER BY starts_at`, ruleID)
	if err != nil {
		t.Fatalf("read occurrences: %v", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan occurrence: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// feedCardsOfRule is what a guest actually sees of ONE series on the main
// screen. Since 2026-08-19 the feed collapses a recurrence the same way the
// public Афиша does, so this is expected to hold at most one id.
func (h *feedHarness) feedCardsOfRule(t *testing.T, ruleID uuid.UUID) []uuid.UUID {
	t.Helper()
	mine := map[uuid.UUID]bool{}
	for _, id := range h.occurrenceIDs(t, ruleID) {
		mine[id] = true
	}
	var out []uuid.UUID
	for id := range h.feedEventIDs(t) {
		if mine[id] {
			out = append(out, id)
		}
	}
	return out
}

// nearestUpcoming is what the collapsed card MUST be: the soonest date of the
// series that a guest can still attend.
func (h *feedHarness) nearestUpcoming(t *testing.T, ruleID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.pool.QueryRow(h.ctx,
		`SELECT id FROM events
		  WHERE recurrence_id = $1 AND status = 'published'
		    AND feed_status = 'approved' AND ends_at > $2
		  ORDER BY starts_at, id LIMIT 1`, ruleID, h.now).Scan(&id); err != nil {
		t.Fatalf("read nearest upcoming occurrence: %v", err)
	}
	return id
}

// assertAllApproved states the half of the rule the collapse does NOT change:
// the approval is written onto EVERY generated date. Only the presentation is
// collapsed — tomorrow's date is already approved, it is simply not shown
// today.
func (h *feedHarness) assertAllApproved(t *testing.T, ids []uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		var status string
		if err := h.pool.QueryRow(h.ctx, `SELECT feed_status FROM events WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read feed status of %s: %v", id, err)
		}
		if status != string(domain.FeedApproved) {
			t.Fatalf("occurrence %s should carry feed_status=approved after the series was approved, got %q", id, status)
		}
	}
}

// TestApprovedRuleReachesTheHomeFeed walks the whole path the six production
// rules will walk: generate → nothing on the main screen → the venue asks →
// still nothing → the superadmin approves → every date already generated is
// approved, and the series shows up as exactly ONE card: its nearest upcoming
// date. Dates generated afterwards are born approved and change nothing on the
// screen until their turn comes.
func TestApprovedRuleReachesTheHomeFeed(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	h := newFeedHarness(t, now, 14*24*time.Hour)

	rule := h.dailyRule(t, "Живая музыка")
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	occurrences := h.occurrenceIDs(t, rule.ID)
	if len(occurrences) < 10 {
		t.Fatalf("expected the daily rule to materialise a fortnight, got %d occurrences", len(occurrences))
	}

	// 1. Default behaviour is unchanged: occurrences are born out of the feed.
	if got := h.feedEventIDs(t); len(got) != 0 {
		t.Fatalf("a rule nobody submitted must stay off the main screen, got %d cards", len(got))
	}

	// 2. The venue asks. Asking is not getting.
	if _, err := h.facade.SubmitToFeed(h.ctx, h.venue, rule.ID); err != nil {
		t.Fatalf("SubmitToFeed: %v", err)
	}
	if got := h.feedEventIDs(t); len(got) != 0 {
		t.Fatalf("a pending rule must stay off the main screen, got %d cards", len(got))
	}
	queue, total, err := h.facade.ListFeedQueue(h.ctx, h.superuser, 1, 20)
	if err != nil {
		t.Fatalf("ListFeedQueue: %v", err)
	}
	if total != 1 || len(queue) != 1 || queue[0].ID != rule.ID {
		t.Fatalf("the submitted rule must be in the moderation queue, got total=%d len=%d", total, len(queue))
	}

	// 3. The venue cannot decide about itself — the moderation step is not
	// bypassable by whoever owns the rule.
	if _, err := h.facade.ReviewFeed(h.ctx, h.venue, rule.ID, FeedReviewInput{Approve: true}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue approving its own series must be forbidden, got %v", err)
	}
	if got := h.feedEventIDs(t); len(got) != 0 {
		t.Fatalf("the refused self-approval must have changed nothing, got %d cards", len(got))
	}

	// 4. The superadmin approves ONCE, and the dates already generated follow —
	// in the database. On the screen the fortnight is ONE card, the nearest
	// date: approving six daily rules used to produce ~98 identical cards
	// (incident of 2026-08-19), which is why the feed collapses a series.
	if _, err := h.facade.ReviewFeed(h.ctx, h.superuser, rule.ID, FeedReviewInput{Approve: true}); err != nil {
		t.Fatalf("ReviewFeed: %v", err)
	}
	h.assertAllApproved(t, occurrences)
	cards := h.feedCardsOfRule(t, rule.ID)
	if len(cards) != 1 || cards[0] != h.nearestUpcoming(t, rule.ID) {
		t.Fatalf("the approved series must show exactly one card, the nearest upcoming date %s, got %v",
			h.nearestUpcoming(t, rule.ID), cards)
	}

	// 5. Dates generated AFTER the decision are born approved: the venue does
	// not have to re-submit every week for the rest of the year. A wider
	// window must NOT add cards — it only fills the queue behind the one that
	// is shown.
	h.gen.cfg.Window = 28 * 24 * time.Hour
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate second pass: %v", err)
	}
	grown := h.occurrenceIDs(t, rule.ID)
	if len(grown) <= len(occurrences) {
		t.Fatalf("the wider window should have generated more dates: %d then %d", len(occurrences), len(grown))
	}
	h.assertAllApproved(t, grown)
	cards = h.feedCardsOfRule(t, rule.ID)
	if len(cards) != 1 || cards[0] != h.nearestUpcoming(t, rule.ID) {
		t.Fatalf("a wider generation window must not multiply the cards: want only %s, got %v",
			h.nearestUpcoming(t, rule.ID), cards)
	}
}

// The negative half, in the same database as the positive one: two rules at the
// same venue, one approved and one not, must not leak into each other.
func TestUnapprovedRuleStaysOffTheHomeFeed(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	h := newFeedHarness(t, now, 7*24*time.Hour)

	approved := h.dailyRule(t, "Живая музыка")
	quiet := h.dailyRule(t, "Тихий вечер")
	// Two rules at the same venue would collide on (recurrence_id, starts_at)
	// only if they shared a rule id; they do not, so both series materialise.
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := h.facade.SubmitToFeed(h.ctx, h.venue, approved.ID); err != nil {
		t.Fatalf("SubmitToFeed: %v", err)
	}
	if _, err := h.facade.ReviewFeed(h.ctx, h.superuser, approved.ID, FeedReviewInput{Approve: true}); err != nil {
		t.Fatalf("ReviewFeed: %v", err)
	}

	h.assertAllApproved(t, h.occurrenceIDs(t, approved.ID))
	cards := h.feedCardsOfRule(t, approved.ID)
	if len(cards) != 1 || cards[0] != h.nearestUpcoming(t, approved.ID) {
		t.Fatalf("the approved series must show exactly one card, the nearest upcoming date %s, got %v",
			h.nearestUpcoming(t, approved.ID), cards)
	}
	// The collapse must not swallow the other rule's absence into a false pass:
	// the unsubmitted series contributes zero cards, not "one, collapsed".
	if cards := h.feedCardsOfRule(t, quiet.ID); len(cards) != 0 {
		t.Fatalf("an unsubmitted rule must contribute no card at all, got %v", cards)
	}
}

// A withdrawal that a guest can still see for eight weeks is not a withdrawal:
// taking the series off the main screen must take the dates ahead with it, and
// must leave the past alone.
func TestWithdrawPullsFutureOccurrencesOffTheFeed(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	h := newFeedHarness(t, now, 7*24*time.Hour)

	rule := h.dailyRule(t, "Живая музыка")
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := h.facade.SubmitToFeed(h.ctx, h.venue, rule.ID); err != nil {
		t.Fatalf("SubmitToFeed: %v", err)
	}
	if _, err := h.facade.ReviewFeed(h.ctx, h.superuser, rule.ID, FeedReviewInput{Approve: true}); err != nil {
		t.Fatalf("ReviewFeed: %v", err)
	}
	// A date that already happened, approved back when it was ahead. History is
	// not rewritten by a decision taken today.
	past := uuid.New()
	if _, err := h.pool.Exec(h.ctx,
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, recurrence_id, feed_status)
		 VALUES ($1, $2, 'Живая музыка', $3, $4, 'published', $5, 'approved')`,
		past, h.restaurantID, now.Add(-48*time.Hour), now.Add(-45*time.Hour), rule.ID); err != nil {
		t.Fatalf("seed past occurrence: %v", err)
	}

	if _, err := h.facade.WithdrawFromFeed(h.ctx, h.venue, rule.ID); err != nil {
		t.Fatalf("WithdrawFromFeed: %v", err)
	}
	if got := h.feedEventIDs(t); len(got) != 0 {
		t.Fatalf("after a withdrawal the series must be off the main screen, got %d cards", len(got))
	}
	var pastStatus string
	if err := h.pool.QueryRow(h.ctx, `SELECT feed_status FROM events WHERE id = $1`, past).Scan(&pastStatus); err != nil {
		t.Fatalf("read past occurrence: %v", err)
	}
	if pastStatus != string(domain.FeedApproved) {
		t.Fatalf("a past occurrence must keep its record, got feed_status=%q", pastStatus)
	}
}

// Editing the series text now rewrites the dates that already exist (migration
// 0097) — that is the feature — so the platform's approval of the OLD words
// cannot survive it. This test pins both halves of that trade:
//
//   - the rule goes back to the moderation queue, and the dates it had already
//     put on the main screen leave it, because their words changed;
//   - a date that OWNS its title keeps both its words and its place: nothing
//     changed on it, so there is nothing to re-review.
//
// It replaces the pre-0097 expectation ("an edit never touches an existing
// date"), which the shared-content feature deliberately reverses.
func TestEditingTheTemplateRewritesExistingDatesAndPullsThemOffTheFeed(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	h := newFeedHarness(t, now, 7*24*time.Hour)

	rule := h.dailyRule(t, "Живая музыка")
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := h.facade.SubmitToFeed(h.ctx, h.venue, rule.ID); err != nil {
		t.Fatalf("SubmitToFeed: %v", err)
	}
	if _, err := h.facade.ReviewFeed(h.ctx, h.superuser, rule.ID, FeedReviewInput{Approve: true}); err != nil {
		t.Fatalf("ReviewFeed: %v", err)
	}
	before := h.feedEventIDs(t)
	if len(before) == 0 {
		t.Fatal("precondition: the approved series should be on the main screen")
	}

	// One date of the series has its own title, claimed the way the event
	// editor claims it.
	dates := h.occurrenceIDs(t, rule.ID)
	own := dates[len(dates)-1]
	if _, err := h.pool.Exec(h.ctx,
		`UPDATE events SET title = 'Джазовый вечер', content_overrides = ARRAY['title']::text[]
		 WHERE id = $1`, own); err != nil {
		t.Fatalf("claim the title of one date: %v", err)
	}

	edited, err := h.facade.Update(h.ctx, h.venue, rule.ID, Input{
		Title:            "Совсем другой вечер",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceDaily,
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         rule.StartsOn,
		IsActive:         true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if edited.OccurrenceFeedStatus != domain.FeedPendingReview {
		t.Fatalf("an edited series must go back to the queue, got %q", edited.OccurrenceFeedStatus)
	}

	// The whole point: the dates carry the new words without anyone editing
	// them one by one.
	for _, id := range dates {
		var title, feedStatus string
		if err := h.pool.QueryRow(h.ctx, `SELECT title, feed_status FROM events WHERE id = $1`, id).
			Scan(&title, &feedStatus); err != nil {
			t.Fatalf("read occurrence: %v", err)
		}
		if id == own {
			if title != "Джазовый вечер" {
				t.Fatalf("a date that owns its title must keep it, got %q", title)
			}
			if feedStatus != string(domain.FeedApproved) {
				t.Fatalf("a date nothing changed on must keep its approval, got %q", feedStatus)
			}
			continue
		}
		if title != "Совсем другой вечер" {
			t.Fatalf("date %s did not follow the series, got %q", id, title)
		}
		if feedStatus != string(domain.FeedNotSubmitted) {
			t.Fatalf("a rewritten date must leave the main screen, got %q", feedStatus)
		}
	}

	// And the guest screen agrees: the only card of this series still standing
	// is the one that was not rewritten.
	after := h.feedEventIDs(t)
	for id := range before {
		if id != own && after[id] {
			t.Fatalf("occurrence %s shows unreviewed words on the main screen", id)
		}
	}

	// Dates generated from the unreviewed template are still born off the feed.
	h.gen.cfg.Window = 21 * 24 * time.Hour
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate after edit: %v", err)
	}
	fresh := h.feedEventIDs(t)
	for _, id := range h.occurrenceIDs(t, rule.ID) {
		// `own` is excluded by name, not by "was it on the feed before": the
		// feed shows ONE card per series, so the card before the edit was the
		// nearest date, and `own` is the one that legitimately took its place
		// afterwards — it still carries the words the moderator approved.
		if id != own && !before[id] && fresh[id] {
			t.Fatalf("occurrence %s generated from an unreviewed template reached the main screen", id)
		}
	}
}
