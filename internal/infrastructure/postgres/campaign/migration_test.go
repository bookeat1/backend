package campaign

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// campaignLinksMigrationVersion is the migration this file exercises.
const campaignLinksMigrationVersion = 112

// campaignLinksMigrationFloor is the version just below 0112. campaign_links
// / campaign_link_hits are brand-new tables nothing else in the suite
// truncates mid-run, so flooring one version below is enough (same posture as
// push_campaigns' migration_test.go).
const campaignLinksMigrationFloor = 111

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
	if err := goose.DownToContext(ctx, db, ".", campaignLinksMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", campaignLinksMigrationFloor, err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < campaignLinksMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, campaignLinksMigrationVersion)
	}
}

// TestCampaignLinksMigrationDownUpOnLiveRows pins criterion 3 (spec §4): the
// migration round-trips (up -> down -> up) on a table with live rows without
// erroring, and the seeded am26-flyer row and its promotion_id come back
// intact both times.
func TestCampaignLinksMigrationDownUpOnLiveRows(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)

	downThenUp(t, db)
	assertSeededFlyerLink(t, pool)

	// Put a live row in each table (a second, unrelated link plus one hit) so
	// the round trip below is exercised against non-empty tables, not just
	// the seed.
	var linkID string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO campaign_links (id, slug, campaign, placement, target_url, status)
		 VALUES (gen_random_uuid(), 'am26-stand', 'marathon-almaty-2026', 'stand',
		         'https://bookeat.godetour.link/lQ9BPpUvJc', 'active')
		 RETURNING id`).Scan(&linkID); err != nil {
		t.Fatalf("seed extra live link: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_link_hits (id, link_id, platform) VALUES (gen_random_uuid(), $1, 'ios')`,
		linkID); err != nil {
		t.Fatalf("seed live hit: %v", err)
	}

	// Second round trip: must not error, and the seed row must still be there
	// (a down/up pair must not duplicate or lose the migration's own INSERT).
	downThenUp(t, db)
	assertSeededFlyerLink(t, pool)

	var linkCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM campaign_links WHERE slug = 'am26-flyer'`).Scan(&linkCount); err != nil {
		t.Fatalf("count am26-flyer rows: %v", err)
	}
	if linkCount != 1 {
		t.Fatalf("am26-flyer seeded %d times after two down/up round trips, want 1", linkCount)
	}
}

func assertSeededFlyerLink(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var campaign, placement, targetURL, status, promotionID string
	err := pool.QueryRow(context.Background(),
		`SELECT campaign, placement, target_url, status, promotion_id::text
		 FROM campaign_links WHERE slug = 'am26-flyer'`).
		Scan(&campaign, &placement, &targetURL, &status, &promotionID)
	if err != nil {
		t.Fatalf("seeded am26-flyer row missing: %v", err)
	}
	if campaign != "marathon-almaty-2026" {
		t.Errorf("campaign = %q, want marathon-almaty-2026", campaign)
	}
	if targetURL != "https://bookeat.godetour.link/lQ9BPpUvJc" {
		t.Errorf("target_url = %q, want the Detour link", targetURL)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if promotionID != "6a3736b9-d4e5-4ec6-9ed2-7233476184fd" {
		t.Errorf("promotion_id = %q, want the marathon promo id", promotionID)
	}

	// The seeded promotion_id must actually resolve in `promos` (migration
	// 0107) — a dangling id here would mean the landing's second button
	// silently points nowhere.
	var promoExists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM promos WHERE id = $1)`, promotionID).Scan(&promoExists); err != nil {
		t.Fatalf("check promos row: %v", err)
	}
	if !promoExists {
		t.Fatalf("campaign_links.promotion_id %s does not resolve in promos (migration 0107 not applied?)", promotionID)
	}
}

// TestCampaignLinkHitsIndexedByLinkID pins the FK + index (spec §4/§5): a hit
// for an unknown link is rejected, and the index exists for the per-slug scan
// count query.
func TestCampaignLinkHitsIndexedByLinkID(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO campaign_link_hits (id, link_id, platform) VALUES (gen_random_uuid(), gen_random_uuid(), 'other')`)
	if err == nil {
		t.Fatal("hit for an unknown link_id was accepted, want a foreign key violation")
	}

	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'campaign_link_hits' AND indexdef LIKE '%link_id%'`).
		Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount == 0 {
		t.Fatal("no index on campaign_link_hits(link_id)")
	}
}
