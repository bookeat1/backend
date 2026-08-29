package eventrecurrence

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// contentTables adds the tables this file seeds on top of `tables`: a sold
// ticket is what proves the sync never touches an occurrence's identity.
var contentTables = append([]string{"event_tickets", "bookings"}, tables...)

// occurrenceRow is what the assertions read back: everything a guest sees plus
// the two things that must survive the sync untouched.
type occurrenceRow struct {
	id             uuid.UUID
	title          string
	description    string
	venue          string
	cover          *string
	tags           []string
	overrides      []string
	status         string
	feedStatus     string
	feedReviewedBy *uuid.UUID
	updatedAt      time.Time
}

func readOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) occurrenceRow {
	t.Helper()
	var r occurrenceRow
	r.id = id
	if err := pool.QueryRow(ctx,
		`SELECT title, description, venue, cover_image_url, tags, content_overrides,
		        status, feed_status, feed_reviewed_by, updated_at
		 FROM events WHERE id = $1`, id).
		Scan(&r.title, &r.description, &r.venue, &r.cover, &r.tags, &r.overrides,
			&r.status, &r.feedStatus, &r.feedReviewedBy, &r.updatedAt); err != nil {
		t.Fatalf("read occurrence: %v", err)
	}
	return r
}

// seedOccurrence inserts one date of a series directly, so the test controls
// its content, its overrides and its window without going through the
// generator.
func seedOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	rid, ruleID uuid.UUID, startsAt time.Time, e occurrenceRow) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, recurrence_id, title, description, venue,
			cover_image_url, tags, content_overrides, starts_at, ends_at, status, feed_status,
			feed_reviewed_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		id, rid, ruleID, e.title, e.description, e.venue, e.cover, e.tags, e.overrides,
		startsAt, startsAt.Add(3*time.Hour), e.status, e.feedStatus, e.feedReviewedBy); err != nil {
		t.Fatalf("seed occurrence: %v", err)
	}
	return id
}

func seedRule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	repo := New(pool)
	rec := fullRule(rid)
	rec.Title = "Greek Party"
	rec.Description = "Сиртаки и узо"
	rec.Venue = "терраса"
	cover := "https://cdn.example/greek.jpg"
	rec.CoverImageURL = &cover
	rec.Tags = []string{"Живая музыка"}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return rec.ID
}

// The whole point of the feature: one edit of the series reaches every date
// that has not ended and has not overridden the field.
func TestSyncOccurrenceContentReachesEveryInheritingDate(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	old := occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		cover: strptr("https://cdn.example/greek.jpg"), tags: []string{"Живая музыка"},
		overrides: []string{}, status: "published", feedStatus: "not_submitted",
	}
	var dates []uuid.UUID
	for i := 1; i <= 3; i++ {
		dates = append(dates, seedOccurrence(ctx, t, pool, rid, ruleID,
			now.Add(time.Duration(i)*24*time.Hour), old))
	}

	newCover := "https://cdn.example/greek-v2.jpg"
	n, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title:         "Греческая вечеринка",
		Description:   "Сиртаки, узо и живой бузуки",
		Venue:         "терраса",
		CoverImageURL: &newCover,
		Tags:          []string{"Живая музыка", "Греция"},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != len(dates) {
		t.Fatalf("all %d dates must be rewritten, got %d", len(dates), n)
	}
	for _, id := range dates {
		got := readOccurrence(ctx, t, pool, id)
		if got.title != "Греческая вечеринка" || got.description != "Сиртаки, узо и живой бузуки" {
			t.Fatalf("date %s kept the old words: %+v", id, got)
		}
		if got.cover == nil || *got.cover != newCover {
			t.Fatalf("date %s kept the old poster: %v", id, got.cover)
		}
		if len(got.tags) != 2 || got.tags[1] != "Греция" {
			t.Fatalf("date %s kept the old chips: %v", id, got.tags)
		}
	}
}

// The exception the owner was promised: a date with its own poster keeps it —
// and still follows the series for everything else. The override is per FIELD.
func TestSyncOccurrenceContentLeavesOverriddenFieldsAlone(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	inheriting := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		cover: strptr("https://cdn.example/greek.jpg"), tags: []string{"Живая музыка"},
		overrides: []string{}, status: "published", feedStatus: "not_submitted",
	})
	// This Saturday has its own guest: its own poster and its own text, marked
	// as such. Its venue line is still the series'.
	own := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(48*time.Hour), occurrenceRow{
		title: "Greek Party с Никосом", description: "Гость — Никос", venue: "терраса",
		cover: strptr("https://cdn.example/nikos.jpg"), tags: []string{"Живая музыка"},
		overrides: []string{"title", "description", "cover_image_url"},
		status:    "published", feedStatus: "not_submitted",
	})

	newCover := "https://cdn.example/greek-v2.jpg"
	if _, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Греческая вечеринка", Description: "Сиртаки, узо и живой бузуки",
		Venue: "летняя веранда", CoverImageURL: &newCover, Tags: []string{"Греция"},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	kept := readOccurrence(ctx, t, pool, own)
	if kept.title != "Greek Party с Никосом" || kept.description != "Гость — Никос" {
		t.Fatalf("an overridden date lost its own words: %+v", kept)
	}
	if *kept.cover != "https://cdn.example/nikos.jpg" {
		t.Fatalf("an overridden date lost its own poster: %s", *kept.cover)
	}
	// ...while the fields it never claimed followed the series.
	if kept.venue != "летняя веранда" || len(kept.tags) != 1 || kept.tags[0] != "Греция" {
		t.Fatalf("the inherited fields of an overridden date must still follow the series: %+v", kept)
	}
	if got := readOccurrence(ctx, t, pool, inheriting); got.title != "Греческая вечеринка" {
		t.Fatalf("the inheriting date must have been rewritten: %+v", got)
	}
}

// A date that already happened is history: whatever the venue writes on the
// series afterwards, last month's poster stays what the guests actually saw.
func TestSyncOccurrenceContentNeverRewritesThePast(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	past := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(-30*24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		tags: []string{}, overrides: []string{}, status: "published", feedStatus: "not_submitted",
	})

	n, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Греческая вечеринка", Description: "новый текст", Venue: "терраса", Tags: []string{},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 0 {
		t.Fatalf("a past date must not be rewritten, got %d rows", n)
	}
	if got := readOccurrence(ctx, t, pool, past); got.title != "Greek Party" {
		t.Fatalf("history was rewritten: %+v", got)
	}
}

// An occurrence the platform approved for the main screen carries the OLD
// words' approval. Rewriting it takes the card off the feed (not_submitted, not
// pending_review — eighteen dates must never flood the item queue) and clears
// the reviewer stamp.
func TestSyncOccurrenceContentWithdrawsApprovedDatesFromTheFeed(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()
	reviewer := seedUser(ctx, t, pool)

	approved := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		tags: []string{}, overrides: []string{}, status: "published",
		feedStatus: "approved", feedReviewedBy: &reviewer,
	})
	// The same series, but this date owns every content field: nothing changes
	// on it, so its approval must survive.
	untouched := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(48*time.Hour), occurrenceRow{
		title: "Своя афиша", description: "свой текст", venue: "свой зал",
		cover: strptr("https://cdn.example/own.jpg"), tags: []string{"Своё"},
		overrides: []string{"title", "description", "venue", "cover_image_url", "tags"},
		status:    "published", feedStatus: "approved", feedReviewedBy: &reviewer,
	})

	if _, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Греческая вечеринка", Description: "новый текст", Venue: "терраса", Tags: []string{},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got := readOccurrence(ctx, t, pool, approved)
	if got.feedStatus != "not_submitted" {
		t.Fatalf("a rewritten card must leave the main screen, got %q", got.feedStatus)
	}
	if got.feedReviewedBy != nil {
		t.Fatalf("the reviewer stamp must be cleared with the approval, got %v", got.feedReviewedBy)
	}
	if got.status != "published" {
		t.Fatalf("the sync must not touch the publication status, got %q", got.status)
	}
	if keep := readOccurrence(ctx, t, pool, untouched); keep.feedStatus != "approved" {
		t.Fatalf("a date nothing changed on must keep its approval, got %q", keep.feedStatus)
	}
}

// A sync that changes nothing writes nothing: no updated_at churn, and the
// returned count answers "how many dates actually changed".
func TestSyncOccurrenceContentIsANoOpWhenNothingDiffers(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	content := domain.EventContent{
		Title: "Greek Party", Description: "Сиртаки и узо", Venue: "терраса",
		CoverImageURL: strptr("https://cdn.example/greek.jpg"), Tags: []string{"Живая музыка"},
	}
	id := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), occurrenceRow{
		title: content.Title, description: content.Description, venue: content.Venue,
		cover: content.CoverImageURL, tags: content.Tags, overrides: []string{},
		status: "published", feedStatus: "approved",
	})
	before := readOccurrence(ctx, t, pool, id)

	n, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, content)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 0 {
		t.Fatalf("an identical sync must write nothing, got %d rows", n)
	}
	after := readOccurrence(ctx, t, pool, id)
	if !after.updatedAt.Equal(before.updatedAt) {
		t.Fatalf("updated_at must not be churned by an identical sync")
	}
	if after.feedStatus != "approved" {
		t.Fatalf("an identical sync must not withdraw an approved card, got %q", after.feedStatus)
	}
}

// The invariant that makes this design safe on a live database: a series edit
// changes WORDS, never identity. A sold ticket still points at the same
// occurrence id afterwards.
func TestSyncOccurrenceContentKeepsTicketsAttached(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	date := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		tags: []string{}, overrides: []string{}, status: "published", feedStatus: "not_submitted",
	})
	ticketID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_tickets (id, event_id, restaurant_id, quantity, unit_price_minor,
			total_minor, status, purchase_idempotency_key)
		 VALUES ($1,$2,$3,2,500000,1000000,'paid',$4)`,
		ticketID, date, rid, "key-"+ticketID.String()); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	if _, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Греческая вечеринка", Description: "новый текст", Venue: "терраса", Tags: []string{},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var eventID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT event_id FROM event_tickets WHERE id = $1`, ticketID).
		Scan(&eventID); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if eventID != date {
		t.Fatalf("the ticket must still point at the same occurrence: want %s, got %s", date, eventID)
	}
}

// The same invariant for a booking. bookings.event_id carries no foreign key
// (migration 0004: a booking outlives the event it was made for), so nothing in
// the database would complain if a series edit orphaned it — which is exactly
// why it is asserted here.
func TestSyncOccurrenceContentKeepsBookingsAttached(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	date := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		tags: []string{}, overrides: []string{}, status: "published", feedStatus: "not_submitted",
	})
	bookingID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests,
			starts_at, ends_at, event_id)
		 VALUES ($1,$2,'Гость','+77000000000','+77000000000',2,$3,$4,$5)`,
		bookingID, rid, now.Add(24*time.Hour), now.Add(26*time.Hour), date); err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	if _, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Греческая вечеринка", Description: "новый текст", Venue: "терраса", Tags: []string{},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var eventID *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT event_id FROM bookings WHERE id = $1`, bookingID).
		Scan(&eventID); err != nil {
		t.Fatalf("read booking: %v", err)
	}
	if eventID == nil || *eventID != date {
		t.Fatalf("the booking must still point at the same occurrence: want %s, got %v", date, eventID)
	}
}

// Two series must not bleed into each other: the sync is scoped by rule.
func TestSyncOccurrenceContentTouchesOnlyItsOwnSeries(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleA := seedRule(ctx, t, pool, rid)
	ruleB := seedRule(ctx, t, pool, rid)
	now := time.Now()

	other := seedOccurrence(ctx, t, pool, rid, ruleB, now.Add(24*time.Hour), occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		tags: []string{}, overrides: []string{}, status: "published", feedStatus: "not_submitted",
	})

	if _, err := New(pool).SyncOccurrenceContent(ctx, ruleA, now, domain.EventContent{
		Title: "Другая серия", Venue: "терраса", Tags: []string{},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readOccurrence(ctx, t, pool, other); got.title != "Greek Party" {
		t.Fatalf("another series was rewritten: %+v", got)
	}
}

func strptr(s string) *string { return &s }

// seedUser inserts a bare user row to stand in for the moderator who approved a
// card (events.feed_reviewed_by is an FK).
func seedUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, phone, role) VALUES ($1, $2, 'admin')`,
		id, "+7700"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// venue_i18n travels with venue: it is the SAME editorial decision (see
// domain.EventContentVenue), so a date that inherits the room inherits its
// translations, and a date that owns the room owns them too.
func TestSyncOccurrenceContentCarriesVenueTranslations(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	now := time.Now()

	base := occurrenceRow{
		title: "Greek Party", description: "Сиртаки и узо", venue: "терраса",
		cover: strptr("https://cdn.example/greek.jpg"), tags: []string{"Живая музыка"},
		overrides: []string{}, status: "published", feedStatus: "not_submitted",
	}
	inheriting := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(24*time.Hour), base)

	ownsVenue := base
	ownsVenue.venue = "банкетный зал"
	ownsVenue.overrides = []string{"venue"}
	own := seedOccurrence(ctx, t, pool, rid, ruleID, now.Add(48*time.Hour), ownsVenue)

	n, err := New(pool).SyncOccurrenceContent(ctx, ruleID, now, domain.EventContent{
		Title: "Greek Party", Description: "Сиртаки и узо",
		Venue:         "терраса",
		VenueI18n:     domain.I18n{"ru": "терраса", "kk": "террасса"},
		CoverImageURL: strptr("https://cdn.example/greek.jpg"),
		Tags:          []string{"Живая музыка"},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Only the venue TRANSLATIONS changed, and that alone must be enough to
	// rewrite the inheriting date — a sync that compared the column only would
	// report zero rows and leave a Kazakh guest reading Russian.
	if n != 1 {
		t.Fatalf("only the inheriting date must be rewritten, got %d", n)
	}

	var i18n map[string]string
	if err := pool.QueryRow(ctx, `SELECT venue_i18n FROM events WHERE id = $1`, inheriting).Scan(&i18n); err != nil {
		t.Fatalf("read inheriting date: %v", err)
	}
	if i18n["kk"] != "террасса" {
		t.Fatalf("the inheriting date must receive the series translations, got %v", i18n)
	}

	var ownI18n *string
	var ownVenue string
	if err := pool.QueryRow(ctx,
		`SELECT venue, venue_i18n::text FROM events WHERE id = $1`, own).Scan(&ownVenue, &ownI18n); err != nil {
		t.Fatalf("read overriding date: %v", err)
	}
	if ownVenue != "банкетный зал" || ownI18n != nil {
		t.Fatalf("a date that owns its room must keep it untranslated by the series: %q / %v", ownVenue, ownI18n)
	}
}

// A rule's translations reach the dates it GENERATES, not only the ones it
// later syncs.
func TestInsertOccurrencesCarriesVenueTranslations(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, contentTables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	repo := New(pool)

	rec := fullRule(rid)
	rec.Venue = "терраса"
	rec.VenueI18n = domain.I18n{"ru": "терраса", "kk": "террасса"}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	n, err := repo.InsertOccurrences(ctx, rec, []time.Time{start})
	if err != nil {
		t.Fatalf("insert occurrences: %v", err)
	}
	if n != 1 {
		t.Fatalf("want one generated date, got %d", n)
	}
	var i18n map[string]string
	if err := pool.QueryRow(ctx,
		`SELECT venue_i18n FROM events WHERE recurrence_id = $1`, rec.ID).Scan(&i18n); err != nil {
		t.Fatalf("read generated date: %v", err)
	}
	if i18n["kk"] != "террасса" {
		t.Fatalf("a generated date must inherit the rule's venue translations, got %v", i18n)
	}
}
