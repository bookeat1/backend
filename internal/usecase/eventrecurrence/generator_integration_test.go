package eventrecurrence

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	eventrecurrencerepo "backend-core/internal/infrastructure/postgres/eventrecurrence"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// The generator's guarantees are DATABASE guarantees — the unique index on
// (recurrence_id, starts_at), the ON CONFLICT DO NOTHING it rides on, and the
// tombstone lookup — so they are tested against real Postgres. A fake can agree
// with an idea Postgres rejects.

// recurrenceTables are the tables these tests own, children first.
var recurrenceTables = []string{
	"event_recurrence_skips", "event_images", "events", "event_recurrences", "restaurants",
}

type harness struct {
	pool         *pgxpool.Pool
	repo         domain.EventRecurrenceRepository
	gen          *Generator
	restaurantID uuid.UUID
	ctx          context.Context
}

func newHarness(t *testing.T, venueTimezone string, window time.Duration, now time.Time) *harness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, recurrenceTables...)
	ctx := context.Background()

	rid := uuid.New()
	var tz any
	if venueTimezone != "" {
		tz = venueTimezone
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active, timezone)
		 VALUES ($1, 'Rooftop', 'almaty', 'mid', true, $2)`, rid, tz); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	repo := eventrecurrencerepo.New(pool)
	gen := NewGenerator(repo, GeneratorConfig{
		Window:           window,
		TimezoneFallback: "UTC",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gen.now = func() time.Time { return now }

	return &harness{pool: pool, repo: repo, gen: gen, restaurantID: rid, ctx: ctx}
}

// weeklyRule is «Караоке-битва по средам и четвергам»: 19:00 local, three
// hours, published straight away so it shows up in the Афиша.
func (h *harness) weeklyRule(t *testing.T, startsOn domain.CalendarDate, weekdays ...domain.ISOWeekday) *domain.EventRecurrence {
	t.Helper()
	rec := &domain.EventRecurrence{
		RestaurantID:     h.restaurantID,
		Title:            "Караоке-битва",
		OccurrenceStatus: domain.EventPublished,
		Tags:             []string{"Караоке"},
		Frequency:        domain.RecurrenceWeekly,
		Weekdays:         weekdays,
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         startsOn,
		IsActive:         true,
	}
	if err := h.repo.Create(h.ctx, rec); err != nil {
		t.Fatalf("create recurrence: %v", err)
	}
	return rec
}

// occurrenceStarts reads back what actually landed in `events`, in venue-local
// wall-clock form — the only form a venue ever reasons about.
func (h *harness) occurrenceStarts(t *testing.T, recurrenceID uuid.UUID, loc *time.Location) []string {
	t.Helper()
	rows, err := h.pool.Query(h.ctx,
		`SELECT starts_at FROM events WHERE recurrence_id = $1 ORDER BY starts_at`, recurrenceID)
	if err != nil {
		t.Fatalf("read occurrences: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan occurrence: %v", err)
		}
		out = append(out, ts.In(loc).Format("2006-01-02 15:04 -0700"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate occurrences: %v", err)
	}
	return out
}

func assertEqualStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d: got %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable on this host: %v", name, err)
	}
	return loc
}

// A weekly rule with TWO weekdays materialises exactly the right dates in the
// venue's own zone — the shape Damir hand-inserted by hand this week.
func TestGenerateWeeklyTwoWeekdaysMaterialisesRealRows(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty) // Monday morning
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	created, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if created != 4 {
		t.Fatalf("created %d occurrences, want 4", created)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, almaty),
		"2026-08-19 19:00 +0500",
		"2026-08-20 19:00 +0500",
		"2026-08-26 19:00 +0500",
		"2026-08-27 19:00 +0500",
	)

	// The occurrences are REAL events carrying the template — that is the whole
	// point: a ticket and a capacity cannot hang off a virtual date.
	var title, status string
	var tags []string
	var recurrenceID uuid.UUID
	var endsAt, startsAt time.Time
	if err := h.pool.QueryRow(h.ctx,
		`SELECT title, status, tags, recurrence_id, starts_at, ends_at FROM events
		 WHERE recurrence_id = $1 ORDER BY starts_at LIMIT 1`, rec.ID).
		Scan(&title, &status, &tags, &recurrenceID, &startsAt, &endsAt); err != nil {
		t.Fatalf("read first occurrence: %v", err)
	}
	if title != "Караоке-битва" || status != string(domain.EventPublished) {
		t.Fatalf("template not copied: title=%q status=%q", title, status)
	}
	if len(tags) != 1 || tags[0] != "Караоке" {
		t.Fatalf("tags not copied: %v", tags)
	}
	if recurrenceID != rec.ID {
		t.Fatalf("occurrence points at %s, want %s", recurrenceID, rec.ID)
	}
	if got := endsAt.Sub(startsAt); got != 3*time.Hour {
		t.Fatalf("occurrence lasts %v, want 3h", got)
	}
}

func TestGenerateDaily(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 4*24*time.Hour, now)

	rec := &domain.EventRecurrence{
		RestaurantID:     h.restaurantID,
		Title:            "Утренний бранч",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceDaily,
		StartMinutes:     11 * 60,
		DurationMinutes:  120,
		StartsOn:         domain.CalendarDate{Year: 2026, Month: time.August, Day: 17},
		IsActive:         true,
	}
	if err := h.repo.Create(h.ctx, rec); err != nil {
		t.Fatalf("create recurrence: %v", err)
	}
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// 17 Aug 11:00 is already past at 09:00? No — it is ahead, so it is included;
	// the window ends at 21 Aug 09:00, so 21 Aug 11:00 is NOT.
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, almaty),
		"2026-08-17 11:00 +0500",
		"2026-08-18 11:00 +0500",
		"2026-08-19 11:00 +0500",
		"2026-08-20 11:00 +0500",
	)
}

// A second pass — and a third — must insert NOTHING. This is the property that
// lets the worker tick every five minutes without piling up duplicates.
func TestGenerateIsIdempotent(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	first, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	third, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if first != 4 || second != 0 || third != 0 {
		t.Fatalf("passes created %d/%d/%d, want 4/0/0", first, second, third)
	}
	if got := h.occurrenceStarts(t, rec.ID, almaty); len(got) != 4 {
		t.Fatalf("after three passes there are %d occurrences: %v", len(got), got)
	}
}

// Two passes racing each other must still produce one row per slot: the unique
// index decides, the loser's insert is a no-op. This is what makes a second
// worker instance (or an overlapping tick) safe.
func TestGenerateConcurrentPassesDoNotDuplicate(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			n, err := h.gen.Generate(h.ctx)
			done <- result{n, err}
		}()
	}
	total := 0
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			t.Fatalf("concurrent pass: %v", r.err)
		}
		total += r.n
	}
	if total != 4 {
		t.Fatalf("two concurrent passes created %d rows in total, want 4", total)
	}
	if got := h.occurrenceStarts(t, rec.ID, almaty); len(got) != 4 {
		t.Fatalf("want 4 occurrences after a race, got %d: %v", len(got), got)
	}
}

// A cancelled occurrence — the venue hides the one date it will not host — must
// NOT come back on the next pass, and must not be silently re-published either.
func TestGenerateDoesNotResurrectCancelledOccurrence(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	cancelled := time.Date(2026, time.August, 26, 19, 0, 0, 0, almaty)
	if _, err := h.pool.Exec(h.ctx,
		`UPDATE events SET status = 'hidden', title = 'Отменено' WHERE recurrence_id = $1 AND starts_at = $2`,
		rec.ID, cancelled); err != nil {
		t.Fatalf("cancel occurrence: %v", err)
	}

	n, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Fatalf("second pass created %d rows, want 0", n)
	}
	var count int
	var status, title string
	if err := h.pool.QueryRow(h.ctx,
		`SELECT count(*) FROM events WHERE recurrence_id = $1 AND starts_at = $2`,
		rec.ID, cancelled).Scan(&count); err != nil {
		t.Fatalf("count cancelled slot: %v", err)
	}
	if count != 1 {
		t.Fatalf("cancelled slot holds %d rows, want exactly 1", count)
	}
	if err := h.pool.QueryRow(h.ctx,
		`SELECT status, title FROM events WHERE recurrence_id = $1 AND starts_at = $2`,
		rec.ID, cancelled).Scan(&status, &title); err != nil {
		t.Fatalf("read cancelled slot: %v", err)
	}
	if status != "hidden" || title != "Отменено" {
		t.Fatalf("the generator overwrote a cancelled occurrence: status=%q title=%q", status, title)
	}
}

// The case the unique index cannot cover: the occurrence row is DELETED, so its
// slot is free again. The tombstone written by usecase/events is what keeps it
// deleted.
func TestGenerateDoesNotResurrectDeletedOccurrenceWithSkip(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	removed := time.Date(2026, time.August, 20, 19, 0, 0, 0, almaty)

	// Exactly what usecase/events.Delete does: tombstone first, then delete.
	if err := h.repo.RecordSkip(h.ctx, rec.ID, removed); err != nil {
		t.Fatalf("record skip: %v", err)
	}
	// Recording it twice must be a no-op, not a duplicate-key error.
	if err := h.repo.RecordSkip(h.ctx, rec.ID, removed); err != nil {
		t.Fatalf("record skip twice: %v", err)
	}
	if _, err := h.pool.Exec(h.ctx,
		`DELETE FROM events WHERE recurrence_id = $1 AND starts_at = $2`, rec.ID, removed); err != nil {
		t.Fatalf("delete occurrence: %v", err)
	}

	n, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Fatalf("second pass recreated %d deleted occurrences, want 0", n)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, almaty),
		"2026-08-19 19:00 +0500",
		"2026-08-26 19:00 +0500",
		"2026-08-27 19:00 +0500",
	)
}

// An until-date ends the series. Nothing beyond it is ever generated, however
// many times the worker runs.
func TestGenerateStopsAtUntilDate(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 60*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)
	until := domain.CalendarDate{Year: 2026, Month: time.August, Day: 26}
	rec.UntilDate = &until
	if err := h.repo.Update(h.ctx, rec); err != nil {
		t.Fatalf("set until date: %v", err)
	}

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, almaty),
		"2026-08-19 19:00 +0500",
		"2026-08-20 19:00 +0500",
		"2026-08-26 19:00 +0500",
	)
}

// The rolling window is a hard boundary: an open-ended weekly rule fills eight
// weeks and not a day more, and only moves forward when the clock does.
func TestGenerateRespectsRollingWindow(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 8*7*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := h.occurrenceStarts(t, rec.ID, almaty)
	if len(got) != 8 {
		t.Fatalf("8-week window over a weekly rule must give 8 dates, got %d: %v", len(got), got)
	}
	if got[0] != "2026-08-19 19:00 +0500" || got[7] != "2026-10-07 19:00 +0500" {
		t.Fatalf("window boundaries wrong: first=%s last=%s", got[0], got[7])
	}

	// A week later the window has rolled: exactly one new date appears.
	h.gen.now = func() time.Time { return now.AddDate(0, 0, 7) }
	n, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if n != 1 {
		t.Fatalf("a week later the window must add exactly 1 date, added %d", n)
	}
}

// The venue's OWN zone decides the instant, not the server's and not the
// platform's. Europe/Lisbon crosses a DST boundary inside the window, so the
// wall-clock hour must hold while the UTC offset changes underneath it.
func TestGenerateUsesVenueTimezoneAcrossDST(t *testing.T) {
	lisbon := mustLoad(t, "Europe/Lisbon")
	now := time.Date(2026, time.October, 19, 9, 0, 0, 0, lisbon)
	h := newHarness(t, "Europe/Lisbon", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.October, Day: 19}, 3, 4)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, lisbon),
		"2026-10-21 19:00 +0100",
		"2026-10-22 19:00 +0100",
		"2026-10-28 19:00 +0000", // clocks went back; 19:00 is still 19:00
		"2026-10-29 19:00 +0000",
	)
}

// A rule may override its venue's zone (a satellite event hosted elsewhere).
// The override wins; the venue's zone is ignored.
func TestGenerateRuleTimezoneOverridesVenue(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	tbilisi := mustLoad(t, "Asia/Tbilisi")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 7*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3)
	rec.Timezone = "Asia/Tbilisi"
	if err := h.repo.Update(h.ctx, rec); err != nil {
		t.Fatalf("set rule timezone: %v", err)
	}

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, tbilisi), "2026-08-19 19:00 +0400")
}

// A venue with NO zone of its own legitimately falls back to the platform zone.
// (A venue with a STORED-BUT-BROKEN zone is a different case — see below.)
func TestGenerateFallsBackToPlatformZoneWhenVenueHasNone(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, utc)
	h := newHarness(t, "", 7*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}
	assertEqualStrings(t, h.occurrenceStarts(t, rec.ID, utc), "2026-08-19 19:00 +0000")
}

// A venue whose stored zone cannot be understood is a DATA FAULT, not a reason
// to guess: that rule generates nothing, and every other venue in the same pass
// is still served. Silently substituting the platform zone is how a whole series
// moves by an hour without anybody being told.
func TestGenerateSkipsRuleWithUnusableTimezoneAndServesTheRest(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "EST", 14*24*time.Hour, now) // legacy fixed-offset name
	broken := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3)

	// A second, healthy venue in the same pass.
	healthyID := uuid.New()
	if _, err := h.pool.Exec(h.ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active, timezone)
		 VALUES ($1, 'Healthy', 'almaty', 'mid', true, 'Asia/Almaty')`, healthyID); err != nil {
		t.Fatalf("seed healthy restaurant: %v", err)
	}
	healthy := &domain.EventRecurrence{
		RestaurantID:     healthyID,
		Title:            "Cocktail Wednesday",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceWeekly,
		Weekdays:         []domain.ISOWeekday{3},
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         domain.CalendarDate{Year: 2026, Month: time.August, Day: 17},
		IsActive:         true,
	}
	if err := h.repo.Create(h.ctx, healthy); err != nil {
		t.Fatalf("create healthy recurrence: %v", err)
	}

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate must not fail because one venue has bad data: %v", err)
	}
	if got := h.occurrenceStarts(t, broken.ID, almaty); len(got) != 0 {
		t.Fatalf("a rule with an unusable zone must generate nothing, got %v", got)
	}
	if got := h.occurrenceStarts(t, healthy.ID, almaty); len(got) != 2 {
		t.Fatalf("the healthy venue must still be served, got %v", got)
	}
}

// Deactivating a rule stops the FUTURE and rewrites nothing: the occurrences
// already materialised — including the ones still ahead — stay exactly as they
// are. They may already carry sold tickets.
func TestDeactivatedRuleStopsGeneratingButKeepsOccurrences(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)

	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := h.repo.SetActive(h.ctx, rec.ID, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// The window rolls two weeks forward; nothing new is generated…
	h.gen.now = func() time.Time { return now.AddDate(0, 0, 14) }
	n, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("pass after deactivation: %v", err)
	}
	if n != 0 {
		t.Fatalf("a deactivated rule generated %d occurrences", n)
	}
	// …and the four already-materialised dates are untouched.
	if got := h.occurrenceStarts(t, rec.ID, almaty); len(got) != 4 {
		t.Fatalf("deactivation removed occurrences: %v", got)
	}
}

// Deleting the RULE must not delete occurrences that already happened; they
// simply become ordinary one-off events (ON DELETE SET NULL).
func TestDeletingRuleKeepsPastOccurrences(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3, 4)
	if _, err := h.gen.Generate(h.ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}

	if _, err := h.pool.Exec(h.ctx, `DELETE FROM event_recurrences WHERE id = $1`, rec.ID); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	var count, orphaned int
	if err := h.pool.QueryRow(h.ctx,
		`SELECT count(*), count(*) FILTER (WHERE recurrence_id IS NULL) FROM events
		 WHERE restaurant_id = $1`, h.restaurantID).Scan(&count, &orphaned); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 4 || orphaned != 4 {
		t.Fatalf("after deleting the rule: %d events, %d of them unlinked; want 4 and 4", count, orphaned)
	}
}

// A rule at a DEACTIVATED venue generates nothing: the venue and everything it
// hosts are invisible to guests, so filling its Афиша weeks ahead would only
// pile up rows nobody can see.
func TestGenerateSkipsInactiveVenue(t *testing.T) {
	almaty := mustLoad(t, "Asia/Almaty")
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, almaty)
	h := newHarness(t, "Asia/Almaty", 14*24*time.Hour, now)
	rec := h.weeklyRule(t, domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}, 3)
	if _, err := h.pool.Exec(h.ctx,
		`UPDATE restaurants SET is_active = false WHERE id = $1`, h.restaurantID); err != nil {
		t.Fatalf("deactivate venue: %v", err)
	}

	n, err := h.gen.Generate(h.ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if n != 0 {
		t.Fatalf("a rule at an inactive venue generated %d occurrences", n)
	}
	if got := h.occurrenceStarts(t, rec.ID, almaty); len(got) != 0 {
		t.Fatalf("want no occurrences, got %v", got)
	}
}
