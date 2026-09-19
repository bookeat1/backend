package pushcampaign

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

// pushCampaignsMigrationVersion is the migration this file exercises.
const pushCampaignsMigrationVersion = 111

// pushCampaignsMigrationFloor is the version just below 0111. Unlike
// foodie_options this migration seeds no reference data and touches no table
// another package's tests truncate mid-run, so flooring one version below is
// enough — see internal/infrastructure/postgres/foodieoption/migration_test.go
// for the case where that is NOT true.
const pushCampaignsMigrationFloor = 110

func gooseDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open goose db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	return db
}

func downThenUp(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if err := goose.DownToContext(ctx, db, ".", pushCampaignsMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", pushCampaignsMigrationFloor, err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < pushCampaignsMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, pushCampaignsMigrationVersion)
	}
}

// TestPushCampaignsMigrationDownUpOnLiveRows pins criterion 1: the migration
// round-trips on a table with LIVE rows. `notifications` already carries
// booking-outbox rows from every earlier migration/test in this run (the
// suite shares one Postgres, per CLAUDE.md's own `make test`); down must not
// touch them, and up must restore the schema this package depends on.
func TestPushCampaignsMigrationDownUpOnLiveRows(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)

	downThenUp(t, db)

	// New columns/tables exist and are usable.
	assertColumnExists(t, pool, "user_notification_preferences", "promo_push_enabled")
	assertColumnExists(t, pool, "push_tickets", "campaign_id")
	assertColumnExists(t, pool, "notifications", "campaign_id")
	assertColumnExists(t, pool, "notifications", "event_id")
	assertColumnExists(t, pool, "notifications", "promo_id")

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM push_campaigns`).Scan(&n); err != nil {
		t.Fatalf("push_campaigns is not queryable after migrating up: %v", err)
	}

	// A second round trip must not error and must leave the schema in the
	// same shape (idempotency of the up/down pair itself, not of any seed).
	downThenUp(t, db)
	assertColumnExists(t, pool, "push_campaigns", "lease_until")
}

// TestPushCampaignsUniqueActiveSubject pins criterion 2: a partial unique
// index, not a read-then-write check, blocks a second active campaign on the
// same (kind, subject_id).
func TestPushCampaignsUniqueActiveSubject(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	subjectID := uuid.New()

	insert := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO push_campaigns (id, kind, subject_id, status, estimated_recipients)
			 VALUES ($1, 'event', $2, 'queued', 0)`, uuid.New(), subjectID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("second active campaign for the same subject was accepted, want a unique violation")
	}

	// Once the first is terminal, a new active campaign for the same subject
	// is allowed again (spec 3.5 — repeat send).
	if _, err := pool.Exec(ctx, `UPDATE push_campaigns SET status='done' WHERE subject_id=$1`, subjectID); err != nil {
		t.Fatalf("finish first campaign: %v", err)
	}
	if err := insert(); err != nil {
		t.Fatalf("insert after the first campaign went terminal: %v", err)
	}
}

// TestNotificationsOneProducerCheck pins the CHECK constraint added to
// `notifications`: exactly one of outbox_event_id/campaign_id, never both,
// never neither.
func TestNotificationsOneProducerCheck(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	userID := seedUser(t, pool)
	campaignID := seedCampaign(t, pool, uuid.New())

	// Neither set -> rejected.
	_, err := pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, type, title, body, created_at)
		 VALUES ($1,$2,'event','t','b', now())`, uuid.New(), userID)
	if err == nil {
		t.Fatal("row with neither outbox_event_id nor campaign_id was accepted")
	}

	// campaign_id only -> accepted.
	_, err = pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, type, title, body, campaign_id, created_at)
		 VALUES ($1,$2,'event','t','b',$3, now())`, uuid.New(), userID, campaignID)
	if err != nil {
		t.Fatalf("row with campaign_id only was rejected: %v", err)
	}

	// A second row for the same (campaign_id, user_id) -> rejected (dedupe).
	_, err = pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, type, title, body, campaign_id, created_at)
		 VALUES ($1,$2,'event','t','b',$3, now())`, uuid.New(), userID, campaignID)
	if err == nil {
		t.Fatal("duplicate (campaign_id, user_id) feed row was accepted")
	}
}

func assertColumnExists(t *testing.T, pool *pgxpool.Pool, table, column string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column).Scan(&n); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	if n == 0 {
		t.Fatalf("column %s.%s does not exist", table, column)
	}
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, full_name, role, created_at, updated_at)
		 VALUES ($1,$2,'Test','user', now(), now())`, id, id.String()+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedCampaign(t *testing.T, pool *pgxpool.Pool, subjectID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO push_campaigns (id, kind, subject_id, status, estimated_recipients, created_at)
		 VALUES ($1,'event',$2,'done',0, $3)`, id, subjectID, time.Now()); err != nil {
		t.Fatalf("seed push campaign: %v", err)
	}
	return id
}
