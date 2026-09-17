package foodieoption

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

// foodieOptionsMigrationVersion is the migration this file exercises.
const foodieOptionsMigrationVersion = 110

// foodieOptionsMigrationFloor is the version just below cuisines (0079/0080),
// NOT just below 0110 (78, not 109 — same floor
// internal/infrastructure/postgres/cuisine/migration_test.go uses for the
// same reason). 0110's own seed LINKS its cuisine tiles to the `cuisines`
// dictionary by CODE (foodie_option_cuisines, spec
// foodie-profile-admin-dictionaries-20260916.md 🔴1 = A) — if the round trip
// only reverted to 109, `cuisines` would keep whatever content another
// package's tests happened to leave it in (this package's own DownToContext
// does not touch it, and `go test ./...` genuinely shares one Postgres
// across every package per CLAUDE.md's own `make test`/CI command — this is
// not a local-only artifact). Flooring at 78 makes 0079/0080 rebuild
// `cuisines` from scratch in the SAME Up call, so this test's assertions
// about the tile -> cuisine-code links hold regardless of what ran before it.
const foodieOptionsMigrationFloor = 78

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
	if err := goose.DownToContext(ctx, db, ".", foodieOptionsMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", foodieOptionsMigrationFloor, err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < foodieOptionsMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, foodieOptionsMigrationVersion)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestFoodieOptionsMigrationDownUpAndSeed pins criterion 1/2 of the spec: the
// migration seeds exactly 36 rows (15/10/8/3 per kind), matching the
// pre-migration Go constants byte for byte on code/kind, and the cuisine-tile
// links match domain.FoodieCuisineDictionaryCodes as it stood on 2026-09-16 —
// including the "indian" tile code that is NOT in the cuisines dictionary on
// this environment and must be silently skipped, never fail the migration.
func TestFoodieOptionsMigrationDownUpAndSeed(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)

	// Round trip once up front so the assertions below run against a fresh,
	// reproducible seed regardless of what earlier tests in this run did.
	downThenUp(t, db)

	if got := countRows(t, pool, `SELECT count(*) FROM foodie_options`); got != 36 {
		t.Fatalf("foodie_options rows = %d, want 36", got)
	}
	wantPerKind := map[string]int{"cuisine": 15, "diet": 10, "allergy": 8, "budget": 3}
	for kind, want := range wantPerKind {
		if got := countRows(t, pool, `SELECT count(*) FROM foodie_options WHERE kind = $1`, kind); got != want {
			t.Errorf("foodie_options kind=%s rows = %d, want %d", kind, got, want)
		}
	}

	// Every row is born active — hidden is something an admin does, not the
	// seed.
	if got := countRows(t, pool, `SELECT count(*) FROM foodie_options WHERE NOT is_active`); got != 0 {
		t.Errorf("%d seeded rows are inactive, want 0", got)
	}

	// no_diet must exist as a diet — it is the one code the API can never let
	// be hidden (criterion 8/state machine, enforced in the usecase, but it
	// has to be seeded in the first place).
	var noDietKind string
	if err := pool.QueryRow(context.Background(),
		`SELECT kind FROM foodie_options WHERE code = 'no_diet'`).Scan(&noDietKind); err != nil {
		t.Fatalf("read no_diet: %v", err)
	}
	if noDietKind != "diet" {
		t.Errorf("no_diet kind = %q, want diet", noDietKind)
	}

	// Cuisine-tile links (🔴1 = A), criterion 2: asian -> pan_asian, japanese
	// (NOT indian — that cuisine code does not exist on this environment and
	// must be silently dropped, not block the migration).
	assertTileLinks(t, pool, "kazakh", []string{"kazakh"})
	assertTileLinks(t, pool, "asian", []string{"japanese", "pan_asian"})
	assertTileLinks(t, pool, "european", []string{"european", "french", "greek", "mediterranean"})
	assertTileLinks(t, pool, "japanese", []string{"japanese"})
	assertTileLinks(t, pool, "italian", []string{"italian"})
	assertTileLinks(t, pool, "seafood", []string{"seafood"})
	assertTileLinks(t, pool, "vegan", []string{"vegan"})
	// Eight tiles with no dictionary mapping at all (§0 answer 1 of the
	// parent spec): korean/meat/desserts/coffee/healthy/fastfood/spicy/bbq.
	for _, code := range []string{"korean", "meat", "desserts", "coffee", "healthy", "fastfood", "spicy", "bbq"} {
		assertTileLinks(t, pool, code, nil)
	}

	// Budget tiers: budget/mid/premium price_category, criterion 2.
	wantPriceCategory := map[string]string{"budget": "₸", "mid": "₸₸", "premium": "₸₸₸"}
	for code, want := range wantPriceCategory {
		var got string
		if err := pool.QueryRow(context.Background(),
			`SELECT price_category FROM foodie_options WHERE kind = 'budget' AND code = $1`, code).Scan(&got); err != nil {
			t.Fatalf("read price_category for %s: %v", code, err)
		}
		if got != want {
			t.Errorf("budget %s price_category = %q, want %q", code, got, want)
		}
	}

	// Idempotency: a second full round trip must leave the same row counts.
	beforeOptions := countRows(t, pool, `SELECT count(*) FROM foodie_options`)
	beforeLinks := countRows(t, pool, `SELECT count(*) FROM foodie_option_cuisines`)
	downThenUp(t, db)
	if got := countRows(t, pool, `SELECT count(*) FROM foodie_options`); got != beforeOptions {
		t.Errorf("foodie_options after a second round trip = %d, want %d", got, beforeOptions)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM foodie_option_cuisines`); got != beforeLinks {
		t.Errorf("foodie_option_cuisines after a second round trip = %d, want %d", got, beforeLinks)
	}
}

// assertTileLinks checks the cuisine-dictionary codes linked to the cuisine
// tile tileCode, ignoring order (the link table carries no position — the
// tile's own contribution to CuisineCodes is a set, see
// domain.MapFoodieCuisinesToDictionaryCodes).
func assertTileLinks(t *testing.T, pool *pgxpool.Pool, tileCode string, want []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.code FROM foodie_option_cuisines fo
		   JOIN foodie_options o ON o.id = fo.option_id
		   JOIN cuisines c ON c.id = fo.cuisine_id
		  WHERE o.kind = 'cuisine' AND o.code = $1
		  ORDER BY c.code`, tileCode)
	if err != nil {
		t.Fatalf("read tile links for %q: %v", tileCode, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("scan tile link for %q: %v", tileCode, err)
		}
		got = append(got, code)
	}
	if len(got) != len(want) {
		t.Errorf("tile %q cuisine links = %v, want %v", tileCode, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("tile %q cuisine links = %v, want %v", tileCode, got, want)
			return
		}
	}
}
