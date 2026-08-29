package eventrecurrence

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// seriesContentMigrationVersion is the migration this file exercises. Down/Up
// runs against the SHARED test database, so the helper always ends with an Up
// and asserts the database is back at the latest version — a test that left it
// at 96 would fail every other package in the suite.
const seriesContentMigrationVersion = 97

// seriesContentMigrationFloor is the version immediately below 0097. The round
// trip reverts down TO this version instead of "one step down": goose's Down
// takes off the LAST applied migration, which stopped being 0097 the moment
// 0098 landed — the rollback then removed somebody else's migration, 0097 was
// never re-applied, and its backfill was never exercised at all. Reverting to a
// floor keeps the step pointing at 0097 no matter how much is stacked on top
// (same rule as cuisineMigrationFloor / featuresMigrationFloor).
const seriesContentMigrationFloor = seriesContentMigrationVersion - 1

func gooseDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open goose db: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	// Put the SHARED database back at the latest version whatever happens here,
	// including a t.Fatal in the middle of a down/up dance. Without this, a
	// failing assertion between the rollback and the re-apply leaves the
	// database one migration short, and every package that runs afterwards
	// fails on a column that is missing for reasons of its own. The assertions
	// themselves are untouched: this only guarantees the cleanup the happy path
	// already does.
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("restore the shared test database: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// downMutateUp rolls 0097 back, seeds the PRE-migration state while the column
// does not exist, and applies it again. Seeding has to happen in that window:
// with the migration applied there is no way to produce a series whose dates
// carry no markers at all, which is exactly the state production is in.
func downMutateUp(t *testing.T, db *sql.DB, mutate func()) {
	t.Helper()
	ctx := context.Background()
	if err := goose.DownToContext(ctx, db, ".", seriesContentMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", seriesContentMigrationFloor, err)
	}
	if mutate != nil {
		mutate()
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < seriesContentMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, seriesContentMigrationVersion)
	}
}

// seedLegacyOccurrence inserts a date the way the generator did BEFORE 0097:
// a full copy of the content, no marker column at all.
func seedLegacyOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	rid, ruleID uuid.UUID, startsAt time.Time, title, description string, cover *string, tags []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, recurrence_id, title, description, venue,
			cover_image_url, tags, starts_at, ends_at, status)
		 VALUES ($1,$2,$3,$4,$5,'терраса',$6,$7,$8,$9,'published')`,
		id, rid, ruleID, title, description, cover, tags, startsAt, startsAt.Add(3*time.Hour)); err != nil {
		t.Fatalf("seed legacy occurrence: %v", err)
	}
	return id
}

// TestMigration0097BackfillsExistingSeries is the production rehearsal: six
// series exist, the biggest of them («Афиша Greek Party») is eighteen dates
// that were filled in one by one. After the migration the series must speak for
// the best-filled date, every other date must keep every character it had, and
// the ones that differ must SAY they differ — otherwise the first edit of the
// series would silently overwrite them.
func TestMigration0097BackfillsExistingSeries(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, contentTables...)

	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	// The rule as production has it: created early, its template never
	// maintained afterwards — the real content lives on the dates.
	ruleID := seedRule(ctx, t, pool, rid)
	if _, err := pool.Exec(ctx,
		`UPDATE event_recurrences SET title = 'черновик', description = '', venue = '',
		        cover_image_url = NULL, tags = '{}' WHERE id = $1`, ruleID); err != nil {
		t.Fatalf("blank the rule template: %v", err)
	}

	poster := "https://cdn.example/greek.jpg"
	ownPoster := "https://cdn.example/nikos.jpg"
	var anchor, plain, own, past uuid.UUID
	var ticketID uuid.UUID

	downMutateUp(t, db, func() {
		now := time.Now()
		// The best-filled date: everything set. This is what the series must
		// end up saying.
		anchor = seedLegacyOccurrence(ctx, t, pool, rid, ruleID, now.Add(72*time.Hour),
			"Greek Party", "Сиртаки и узо", &poster, []string{"Живая музыка"})
		// An identical date — same words, fewer of them filled in.
		plain = seedLegacyOccurrence(ctx, t, pool, rid, ruleID, now.Add(96*time.Hour),
			"Greek Party", "Сиртаки и узо", &poster, []string{"Живая музыка"})
		// A date the venue edited by hand: its own guest and its own poster.
		own = seedLegacyOccurrence(ctx, t, pool, rid, ruleID, now.Add(120*time.Hour),
			"Greek Party с Никосом", "Гость — Никос", &ownPoster, []string{"Живая музыка"})
		// A date that already happened, with a paid ticket on it.
		past = seedLegacyOccurrence(ctx, t, pool, rid, ruleID, now.Add(-240*time.Hour),
			"Greek Party", "старый текст", nil, []string{})
		ticketID = uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO event_tickets (id, event_id, restaurant_id, quantity, unit_price_minor,
				total_minor, status, purchase_idempotency_key)
			 VALUES ($1,$2,$3,2,500000,1000000,'paid',$4)`,
			ticketID, past, rid, "key-"+ticketID.String()); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	})

	// 1. The series now says what the best-filled date said.
	var ruleTitle, ruleDesc, ruleVenue string
	var ruleCover *string
	var ruleTags []string
	if err := pool.QueryRow(ctx,
		`SELECT title, description, venue, cover_image_url, tags FROM event_recurrences WHERE id = $1`,
		ruleID).Scan(&ruleTitle, &ruleDesc, &ruleVenue, &ruleCover, &ruleTags); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if ruleTitle != "Greek Party" || ruleDesc != "Сиртаки и узо" || ruleVenue != "терраса" {
		t.Fatalf("the series must speak for its best-filled date, got %q / %q / %q", ruleTitle, ruleDesc, ruleVenue)
	}
	if ruleCover == nil || *ruleCover != poster {
		t.Fatalf("the series poster was not carried over: %v", ruleCover)
	}
	if len(ruleTags) != 1 || ruleTags[0] != "Живая музыка" {
		t.Fatalf("the series chips were not carried over: %v", ruleTags)
	}

	// 2. Every date kept its own content, character for character.
	for id, wantTitle := range map[uuid.UUID]string{
		anchor: "Greek Party",
		plain:  "Greek Party",
		own:    "Greek Party с Никосом",
		past:   "Greek Party",
	} {
		if got := readOccurrence(ctx, t, pool, id); got.title != wantTitle {
			t.Fatalf("date %s lost its content: %q, want %q", id, got.title, wantTitle)
		}
	}
	if got := readOccurrence(ctx, t, pool, own); *got.cover != ownPoster {
		t.Fatalf("the hand-edited date lost its own poster: %s", *got.cover)
	}

	// 3. The dates that agree with the series inherit; the one that differs
	//    owns exactly the fields in which it differs.
	if got := readOccurrence(ctx, t, pool, anchor); len(got.overrides) != 0 {
		t.Fatalf("the anchor IS the series content and must own nothing, got %v", got.overrides)
	}
	if got := readOccurrence(ctx, t, pool, plain); len(got.overrides) != 0 {
		t.Fatalf("an identical date must inherit, got %v", got.overrides)
	}
	gotOwn := readOccurrence(ctx, t, pool, own)
	if len(gotOwn.overrides) != 3 ||
		gotOwn.overrides[0] != "title" || gotOwn.overrides[1] != "description" ||
		gotOwn.overrides[2] != "cover_image_url" {
		t.Fatalf("the hand-edited date must own exactly what it changed, got %v", gotOwn.overrides)
	}
	gotPast := readOccurrence(ctx, t, pool, past)
	if len(gotPast.overrides) != 3 {
		t.Fatalf("the past date differs in text, poster and chips and must say so, got %v", gotPast.overrides)
	}

	// 4. Nothing moved: the paid ticket still points at the same occurrence.
	var eventID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT event_id FROM event_tickets WHERE id = $1`, ticketID).
		Scan(&eventID); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if eventID != past {
		t.Fatalf("the migration must not touch a ticket's event: want %s, got %s", past, eventID)
	}
}

// A rollback must be survivable on a populated database: the dates keep their
// content, the rules get their pre-0097 template back, and the whole thing can
// be applied again — which is what a rollback-and-retry deploy does.
func TestMigration0097RollsBackAndReapplies(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, contentTables...)

	rid := seedRestaurant(ctx, t, pool, "Greek", "Asia/Almaty")
	ruleID := seedRule(ctx, t, pool, rid)
	if _, err := pool.Exec(ctx,
		`UPDATE event_recurrences SET title = 'шаблон до 0097' WHERE id = $1`, ruleID); err != nil {
		t.Fatalf("set template: %v", err)
	}
	var date uuid.UUID
	downMutateUp(t, db, func() {
		date = seedLegacyOccurrence(ctx, t, pool, rid, ruleID, time.Now().Add(48*time.Hour),
			"Greek Party", "Сиртаки и узо", nil, []string{})
	})

	// Roll back once more and check what a rollback actually costs.
	if err := goose.DownToContext(ctx, db, ".", seriesContentMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", seriesContentMigrationFloor, err)
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM events WHERE id = $1`, date).Scan(&title); err != nil {
		t.Fatalf("read date after rollback: %v", err)
	}
	if title != "Greek Party" {
		t.Fatalf("a rollback must not touch a date's content, got %q", title)
	}
	if err := pool.QueryRow(ctx, `SELECT title FROM event_recurrences WHERE id = $1`, ruleID).
		Scan(&title); err != nil {
		t.Fatalf("read rule after rollback: %v", err)
	}
	if title != "шаблон до 0097" {
		t.Fatalf("a rollback must restore the pre-0097 template, got %q", title)
	}
	var backupExists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.event_recurrence_content_backup_0097') IS NOT NULL`).
		Scan(&backupExists); err != nil {
		t.Fatalf("check backup table: %v", err)
	}
	if backupExists {
		t.Fatal("the rollback must drop its own backup table")
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up after rollback: %v", err)
	}
}
