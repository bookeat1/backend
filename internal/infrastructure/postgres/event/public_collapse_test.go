package event

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// The collapse of a recurring series into ONE card is a SQL rule (a window
// function plus a count over the collapsed set), so it is tested against real
// Postgres. A fake would only re-run the Go code that has no say here.

var collapseTables = []string{"event_recurrence_skips", "event_images", "events", "event_recurrences", "restaurants"}

// seedRule inserts a recurrence rule directly: this package tests the events
// repository, and going through the recurrence repository would only add a
// dependency on a package whose behaviour is irrelevant here.
func seedRule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_recurrences (id, restaurant_id, title, frequency, weekdays,
			start_minutes, duration_minutes, starts_on, is_active)
		 VALUES ($1, $2, $3, 'daily', '{}', 1140, 180, current_date, true)`,
		id, rid, title); err != nil {
		t.Fatalf("seed recurrence: %v", err)
	}
	return id
}

// seedOccurrence creates an event belonging to a rule. Create() does not write
// recurrence_id (only the generator does), so the link is set right after.
func seedOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo *Repository, rid, ruleID uuid.UUID, title string, startsIn time.Duration) uuid.UUID {
	t.Helper()
	e := mkEvent(rid, domain.EventPublished, startsIn, 2*time.Hour)
	e.Title = title
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create occurrence: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET recurrence_id = $2 WHERE id = $1`, e.ID, ruleID); err != nil {
		t.Fatalf("link occurrence to rule: %v", err)
	}
	return e.ID
}

// TestListPublicUpcoming_RecurringCollapsesToNearestFutureOccurrence is the
// «Живая музыка в ресторане INZHU» case: a daily rule filled the Афиша with 55
// identical cards. The series must contribute exactly one card, and it must be
// the NEAREST one still ahead — not the first ever generated, and never a date
// that already passed.
func TestListPublicUpcoming_RecurringCollapsesToNearestFutureOccurrence(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "INZHU")
	repo := New(pool)
	ruleID := seedRule(ctx, t, pool, rid, "Живая музыка")

	// Yesterday's occurrence: already ended, must not be picked as "nearest".
	seedOccurrence(ctx, t, pool, repo, rid, ruleID, "вчера", -26*time.Hour)
	nearest := seedOccurrence(ctx, t, pool, repo, rid, ruleID, "сегодня", 3*time.Hour)
	seedOccurrence(ctx, t, pool, repo, rid, ruleID, "завтра", 27*time.Hour)
	seedOccurrence(ctx, t, pool, repo, rid, ruleID, "послезавтра", 51*time.Hour)

	// A one-off event at the same venue is untouched by the rule.
	oneOff := mkEvent(rid, domain.EventPublished, 6*time.Hour, 2*time.Hour)
	oneOff.Title = "Винный ужин"
	if err := repo.Create(ctx, oneOff); err != nil {
		t.Fatalf("create one-off: %v", err)
	}

	items, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("expected 2 cards (one series + one one-off), got total=%d len=%d", total, len(items))
	}
	// Soonest first: the nearest occurrence (in 3h) before the one-off (in 6h).
	if items[0].ID != nearest {
		t.Fatalf("expected the nearest FUTURE occurrence %s first, got %s (%s)", nearest, items[0].ID, items[0].Title)
	}
	if items[1].ID != oneOff.ID {
		t.Fatalf("expected the one-off event to be listed as usual, got %s", items[1].ID)
	}
}

// A one-off event is never touched by the collapse, whatever else is in the
// list: every event with a NULL recurrence_id must survive it. Without the
// explicit `recurrence_id IS NULL` branch they would all share one window
// partition and 19 of 20 would silently vanish.
func TestListPublicUpcoming_OneOffEventsAreAllListed(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	for i := 0; i < 5; i++ {
		e := mkEvent(rid, domain.EventPublished, time.Duration(i+1)*24*time.Hour, 2*time.Hour)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("create one-off: %v", err)
		}
	}

	items, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if total != 5 || len(items) != 5 {
		t.Fatalf("expected all 5 one-off events, got total=%d len=%d", total, len(items))
	}
}

// The total must describe the COLLAPSED list, otherwise the app shows a card
// count it can never reach and pages into empty responses.
func TestListPublicUpcoming_TotalAndPaginationAgreeWithCollapsedList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Rooftop")
	repo := New(pool)

	// Two series of three dates each, plus two one-off events: four cards.
	ruleA := seedRule(ctx, t, pool, rid, "Караоке")
	ruleB := seedRule(ctx, t, pool, rid, "Джаз")
	for i := 1; i <= 3; i++ {
		seedOccurrence(ctx, t, pool, repo, rid, ruleA, "Караоке", time.Duration(i)*24*time.Hour)
		seedOccurrence(ctx, t, pool, repo, rid, ruleB, "Джаз", time.Duration(i)*36*time.Hour)
	}
	for i := 0; i < 2; i++ {
		e := mkEvent(rid, domain.EventPublished, time.Duration(i+1)*time.Hour, 2*time.Hour)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("create one-off: %v", err)
		}
	}

	now := time.Now()
	_, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{PerPage: 2}, now)
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected the collapsed total 4 (2 series + 2 one-off), got %d", total)
	}

	seen := map[uuid.UUID]bool{}
	for page := 1; page <= 3; page++ {
		items, pageTotal, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{Page: page, PerPage: 2}, now)
		if err != nil {
			t.Fatalf("ListPublicUpcoming page %d: %v", page, err)
		}
		if pageTotal != 4 {
			t.Fatalf("page %d reported total %d, want 4", page, pageTotal)
		}
		for _, it := range items {
			if seen[it.ID] {
				t.Fatalf("event %s returned on two pages", it.ID)
			}
			seen[it.ID] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("walking the pages returned %d distinct cards, but total says 4", len(seen))
	}
}

// «Nearest» is nearest INSIDE the filtered window: a guest asking for next week
// gets that week's occurrence, not the one happening tomorrow.
func TestListPublicUpcoming_CollapseRespectsTheDateFilter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Rooftop")
	repo := New(pool)
	ruleID := seedRule(ctx, t, pool, rid, "Караоке")

	seedOccurrence(ctx, t, pool, repo, rid, ruleID, "завтра", 24*time.Hour)
	wanted := seedOccurrence(ctx, t, pool, repo, rid, ruleID, "через неделю", 7*24*time.Hour)
	seedOccurrence(ctx, t, pool, repo, rid, ruleID, "через две", 14*24*time.Hour)

	from := time.Now().Add(5 * 24 * time.Hour)
	to := time.Now().Add(10 * 24 * time.Hour)
	items, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{From: &from, To: &to}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly one card inside the window, got total=%d len=%d", total, len(items))
	}
	if items[0].ID != wanted {
		t.Fatalf("expected the first occurrence INSIDE the filter %s, got %s", wanted, items[0].ID)
	}
}

// The cabinet is not a guest catalog: a venue edits or cancels ONE date, so the
// admin listing must keep showing every generated occurrence.
func TestListByRestaurant_AdminStillSeesEveryOccurrence(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, collapseTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "INZHU")
	repo := New(pool)
	ruleID := seedRule(ctx, t, pool, rid, "Живая музыка")

	for i := 1; i <= 6; i++ {
		seedOccurrence(ctx, t, pool, repo, rid, ruleID, "Живая музыка", time.Duration(i)*24*time.Hour)
	}

	items, total, err := repo.ListByRestaurant(ctx, rid, nil, 1, 50)
	if err != nil {
		t.Fatalf("ListByRestaurant: %v", err)
	}
	if total != 6 || len(items) != 6 {
		t.Fatalf("cabinet must list every occurrence: got total=%d len=%d, want 6", total, len(items))
	}

	// And so must the per-venue public listing on the restaurant's own page —
	// this PR narrows the cross-venue Афиша only.
	_, publicTotal, err := repo.ListPublishedUpcoming(ctx, rid, time.Now(), 1, 50)
	if err != nil {
		t.Fatalf("ListPublishedUpcoming: %v", err)
	}
	if publicTotal != 6 {
		t.Fatalf("venue page listing changed unexpectedly: got %d, want 6", publicTotal)
	}
}
