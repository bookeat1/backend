package appversion

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

// policyMigrationVersion is the migration this file exercises.
const policyMigrationVersion = 103

// policyMigrationFloor is the version immediately below it. The round trip
// reverts down TO the floor rather than "one step down": goose's Down takes off
// the LAST applied migration, and 0103 stops being the last one the moment 0104
// lands — from then on a plain Down would roll back a STRANGER's migration and
// this test would quietly measure nothing. That exact rot has already happened
// in this repository once (see the cities/eventrecurrence floors).
const policyMigrationFloor = policyMigrationVersion - 1

// gooseDB opens goose's database/sql handle onto the same TEST_DATABASE_URL the
// pgx pool uses, and guarantees the SHARED database is put back at the latest
// version whatever happens here — including a t.Fatal between the rollback and
// the re-apply, which would otherwise leave every later package a table short.
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

// TestMigration0103RollsBackAndReapplies runs the operation a rollback-and-retry
// deploy performs, on a table that ALREADY HAS LIVE ROWS with a configured
// threshold — the state production would be in when somebody rolls back an
// unrelated later migration.
//
// Two properties are asserted, and the second is the one that matters at 3am:
//
//  1. the DOWN drops the table cleanly even with rows in it (nothing references
//     it, so there is no FK to trip over);
//  2. the re-applied UP comes back with EMPTY thresholds. A rollback of this
//     feature can therefore only ever STOP forcing updates, never start.
func TestMigration0103RollsBackAndReapplies(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)
	ctx := context.Background()
	repo := New(pool)

	// Live rows, with the blocking mode switched on.
	reset(t, pool)
	forced := domain.MobileAppPolicy{
		Platform:            domain.PlatformIOS,
		MinSupportedVersion: "9.9",
		StoreURL:            "https://apps.apple.com/app/id6757542577",
		RequiredTitle:       "Нужно обновить",
		RequiredMessage:     "Обновите приложение",
	}
	if err := repo.Upsert(ctx, &forced); err != nil {
		t.Fatalf("seed a configured policy: %v", err)
	}
	if got, err := repo.Get(ctx, domain.PlatformIOS); err != nil || got.Decide("1.5") != domain.AppUpdateRequired {
		t.Fatalf("the fixture is not actually forcing anything (err=%v)", err)
	}

	if err := goose.DownToContext(ctx, db, ".", policyMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", policyMigrationFloor, err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.mobile_app_policies') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check table after down: %v", err)
	}
	if exists {
		t.Fatal("the rollback left mobile_app_policies behind")
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < policyMigrationVersion {
		t.Fatalf("the database came back at version %d, below %d", version, policyMigrationVersion)
	}

	items, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after the round trip: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d rows after re-applying, want both platforms", len(items))
	}
	for _, p := range items {
		if p.MinSupportedVersion != "" || p.MinRecommendedVersion != "" {
			t.Errorf("%s came back with a threshold (%q / %q): a rollback must never start forcing updates",
				p.Platform, p.MinSupportedVersion, p.MinRecommendedVersion)
		}
		if p.Decide("0.1") != domain.AppUpdateNone {
			t.Errorf("%s still blocks an old build after the round trip", p.Platform)
		}
	}
}

// TestMigration0103IsIdempotentOnAPopulatedTable. The seed uses ON CONFLICT DO
// NOTHING, so re-running the UP over rows an operator has since edited must not
// revert their thresholds or their wording back to the shipped defaults.
func TestMigration0103IsIdempotentOnAPopulatedTable(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	repo := New(pool)
	edited := domain.MobileAppPolicy{
		Platform:              domain.PlatformAndroid,
		MinRecommendedVersion: "1.9",
		StoreURL:              "https://play.google.com/store/apps/details?id=kz.bookeat.app",
		RecommendedTitle:      "Своя формулировка",
		RecommendedMessage:    "Своё сообщение",
	}
	if err := repo.Upsert(ctx, &edited); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Exactly what the seed statement does, run a second time.
	if _, err := pool.Exec(ctx,
		`INSERT INTO mobile_app_policies (platform, store_url, recommended_title, recommended_message)
		 VALUES ('android', 'https://play.google.com/store/apps/details?id=kz.bookeat.app',
		         'Доступно обновление', 'Обновите приложение')
		 ON CONFLICT (platform) DO NOTHING`); err != nil {
		t.Fatalf("re-run the seed: %v", err)
	}

	got, err := repo.Get(ctx, domain.PlatformAndroid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RecommendedTitle != "Своя формулировка" || got.MinRecommendedVersion != "1.9" {
		t.Errorf("re-running the seed overwrote an operator's edit: %+v", *got)
	}
}
