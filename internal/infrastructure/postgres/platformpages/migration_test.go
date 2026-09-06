package platformpages

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// pageMigrationVersion is the migration this file exercises.
const pageMigrationVersion = 105

// pageMigrationFloor is the version immediately below it. Rolling back "one
// step" via goose's Down only means "0105" for as long as 0105 IS the last
// applied migration — the moment a later one lands, a plain Down would roll
// back a STRANGER's migration and this test would measure nothing. Roll down
// TO the floor explicitly instead (see the appversion/cities precedent this
// mirrors).
const pageMigrationFloor = pageMigrationVersion - 1

// gooseDB opens goose's database/sql handle onto the same TEST_DATABASE_URL
// the pgx pool uses, and guarantees the SHARED database is put back at the
// latest version whatever happens here.
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
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("restore the shared test database: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// TestMigration0105RollsBackAndReapplies runs the operation a rollback-and-
// retry deploy performs, on a table that ALREADY HAS A LIVE, PUBLISHED ROW —
// the state production would be in the moment somebody rolls back an
// unrelated later migration after Дамир has already published the offer.
//
// Two properties matter: the DOWN drops the table cleanly with rows in it
// (nothing references it, so there is no FK to trip over), and the
// re-applied UP comes back with the full seven-slug seed, unpublished — a
// rollback of this feature can only ever take a page OFFLINE, never publish
// one that was not before.
func TestMigration0105RollsBackAndReapplies(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)
	ctx := context.Background()
	repo := New(pool)

	reset(t, pool)
	published, err := repo.Get(ctx, domain.PlatformPageOffer)
	if err != nil {
		t.Fatalf("seed fixture Get: %v", err)
	}
	now := published.CreatedAt
	published.Body = "# Оферта\n\nПубличный текст."
	published.PublishedAt = &now
	if err := repo.Update(ctx, published); err != nil {
		t.Fatalf("publish the fixture: %v", err)
	}

	if err := goose.DownToContext(ctx, db, ".", pageMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", pageMigrationFloor, err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.platform_pages') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check table after down: %v", err)
	}
	if exists {
		t.Fatal("the rollback left platform_pages behind")
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < pageMigrationVersion {
		t.Fatalf("the database came back at version %d, below %d", version, pageMigrationVersion)
	}

	items, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after the round trip: %v", err)
	}
	if len(items) != len(domain.PlatformPageSlugs) {
		t.Fatalf("got %d rows after re-applying, want all %d seeded", len(items), len(domain.PlatformPageSlugs))
	}
	for _, p := range items {
		if p.Published() {
			t.Errorf("%s came back published after the round trip: a rollback must never publish anything", p.Slug)
		}
	}
}

// TestMigration0105IsIdempotentOnAPopulatedTable. The seed uses ON CONFLICT
// DO NOTHING, so re-running the UP over rows an operator has since edited
// must not revert their titles, translations or published state back to the
// shipped defaults.
func TestMigration0105IsIdempotentOnAPopulatedTable(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	repo := New(pool)
	edited, err := repo.Get(ctx, domain.PlatformPageAbout)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	now := edited.CreatedAt
	edited.Title = "О компании BookEat"
	edited.Body = "# О нас"
	edited.PublishedAt = &now
	if err := repo.Update(ctx, edited); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Exactly what the seed statement does, run a second time.
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform_pages (slug, title) VALUES ('about', 'О BookEat')
		 ON CONFLICT (slug) DO NOTHING`); err != nil {
		t.Fatalf("re-run the seed: %v", err)
	}

	got, err := repo.Get(ctx, domain.PlatformPageAbout)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "О компании BookEat" || !got.Published() {
		t.Errorf("re-running the seed overwrote an operator's edit: %+v", *got)
	}
}
