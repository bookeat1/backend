package cuisine

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// cuisinesMigrationVersion is the migration this file exercises. Down/Up runs
// against the SHARED test database, so downThenUp always ends with an Up and
// asserts the database is back at the latest version — a test that left it at
// 78 would fail every other package in the suite.
const cuisinesMigrationVersion = 80

// cuisineMigrationFloor is the version JUST BELOW the cuisine feature (0079
// structure + transfer, 0080 the owner-approved layout of the disputed
// spellings). The round trip reverts down TO this version rather than counting
// a fixed number of steps: counting steps silently stops reverting the cuisine
// migrations the moment anything is stacked on top of them, and the test then
// keeps passing while exercising nothing — which is exactly what happened when
// 0081 and 0082 landed.
const cuisineMigrationFloor = 78

// gooseDB opens goose's database/sql handle onto the same TEST_DATABASE_URL the
// pgx pool uses. goose needs *sql.DB; the repositories need a pgx pool. Both
// point at one database, which is the whole point: the assertions below read
// what the migration actually produced, not a Go-side reconstruction of it.
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

// downThenUp reverts every cuisine migration and applies them again, which is
// exactly the operation a rollback-and-retry deploy performs.
func downThenUp(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if err := goose.DownToContext(ctx, db, ".", cuisineMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", cuisineMigrationFloor, err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < cuisinesMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, cuisinesMigrationVersion)
	}
}

// seededVenue is one production cuisine spelling under test. wantCuisines is
// the OWNER-APPROVED outcome (2026-08-25): the exact dictionary codes the
// venue must end up with, in order. An empty list means "deliberately no
// cuisine" — a venue TYPE was written into the cuisine field.
type seededVenue struct {
	id           uuid.UUID
	cuisine      string
	i18n         string
	wantCuisines []string
}

// TestCuisineMigrationsDownUpAndApprovedLayout checks the three things the two
// cuisine migrations promise together: they roll back cleanly, their data
// transfer is idempotent, and they never guess — 0079 maps only what is
// unambiguous, 0080 applies the layout a human actually approved.
//
// The venue rows are seeded with the REAL spellings measured on production on
// 2026-08-25 (all 19 distinct values, read straight from the prod database —
// the public API cannot show them, it serves active venues only) — including
// the composite ones the dictionary exists to fix and the lower-case variant
// that used to be a separate filter value.
func TestCuisineMigrationsDownUpAndApprovedLayout(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurant_cuisines", "restaurants")

	venues := []seededVenue{
		{uuid.New(), "Европейская", `{"en":"European","kk":"Еуропалық"}`, []string{"european"}},
		{uuid.New(), "Итальянская", "", []string{"italian"}},
		// Same cuisine, lower case: before 0079 these were two different
		// filter values and the app sent whichever one it had scraped.
		{uuid.New(), "европейская", "", []string{"european"}},
		// Padded spelling — the alias key is btrim'ed on both sides.
		{uuid.New(), "  Казахская ", "", []string{"kazakh"}},

		// The disputed spellings, laid out as the owner approved on
		// 2026-08-25. Order matters: position 0 is the venue's main cuisine.
		{uuid.New(), "Авторская, европейская", "", []string{"authors", "european"}},
		{uuid.New(), "Европейская, казахская", "", []string{"european", "kazakh"}},
		{uuid.New(), "Морепродукты, европейская", "", []string{"seafood", "european"}},
		// «Кафе» is a venue TYPE, not a cuisine — it is dropped, and exactly
		// one cuisine survives. This is the case an automatic comma split
		// would have got wrong in the opposite direction.
		{uuid.New(), "Кафе, европейская", "", []string{"european"}},
		// A cuisine with a parenthetical clarification: ONE cuisine, not two.
		// This is why splitting on punctuation is forbidden.
		{uuid.New(), "Японская (идзакая)", "", []string{"japanese"}},
		// Venue types written into the cuisine field: no cuisine at all.
		{uuid.New(), "Кофейня", "", nil},
		{uuid.New(), "Винный бар", "", nil},
	}
	for _, v := range venues {
		var i18n any
		if v.i18n != "" {
			i18n = []byte(v.i18n)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, cuisine_type, cuisine_type_i18n, city, price_category)
			 VALUES ($1,'Venue',$2,$3,'Алматы','₸₸')`, v.id, v.cuisine, i18n); err != nil {
			t.Fatalf("seed venue %q: %v", v.cuisine, err)
		}
	}

	db := gooseDB(t)
	downThenUp(t, db)

	// The dictionary itself.
	if got := countRows(t, pool, "cuisines"); got != 14 {
		t.Fatalf("cuisines after re-up = %d, want the 14 seeded entries (12 from 0079 + 2 from 0080)", got)
	}
	// Translations are TAKEN FROM THE DATA, not invented: only the venue that
	// actually carried a cuisine_type_i18n contributes one.
	var euI18n []byte
	if err := pool.QueryRow(ctx, `SELECT name_i18n FROM cuisines WHERE code = 'european'`).Scan(&euI18n); err != nil {
		t.Fatalf("read european i18n: %v", err)
	}
	if len(euI18n) == 0 {
		t.Error("european name_i18n is empty: the existing venue translations were not carried over")
	}
	var itI18n []byte
	if err := pool.QueryRow(ctx, `SELECT name_i18n FROM cuisines WHERE code = 'italian'`).Scan(&itI18n); err != nil {
		t.Fatalf("read italian i18n: %v", err)
	}
	if len(itI18n) != 0 {
		t.Errorf("italian name_i18n = %s, want NULL: no venue supplied one and none may be invented", itI18n)
	}

	assertLinks(t, pool, venues)

	// Idempotency: another full round trip must not duplicate a single row.
	beforeLinks := countRows(t, pool, "restaurant_cuisines")
	beforeAliases := countRows(t, pool, "cuisine_aliases")
	downThenUp(t, db)
	if got := countRows(t, pool, "cuisines"); got != 14 {
		t.Errorf("cuisines after a second round trip = %d, want 14", got)
	}
	if got := countRows(t, pool, "restaurant_cuisines"); got != beforeLinks {
		t.Errorf("restaurant_cuisines after a second round trip = %d, want %d", got, beforeLinks)
	}
	if got := countRows(t, pool, "cuisine_aliases"); got != beforeAliases {
		t.Errorf("cuisine_aliases after a second round trip = %d, want %d", got, beforeAliases)
	}
	assertLinks(t, pool, venues)
}

// assertLinks checks the migrations' central promise: every venue ends up with
// EXACTLY the cuisines the owner approved, in the approved order — and in every
// case the venue's original cuisine_type string is still there, untouched. The
// derived string is rewritten by the application when a venue edits its set,
// never by a migration, which is what makes the whole thing rollback-safe.
func assertLinks(t *testing.T, pool *pgxpool.Pool, venues []seededVenue) {
	t.Helper()
	ctx := context.Background()
	for _, v := range venues {
		rows, err := pool.Query(ctx,
			`SELECT c.code FROM restaurant_cuisines rc
			   JOIN cuisines c ON c.id = rc.cuisine_id
			  WHERE rc.restaurant_id = $1
			  ORDER BY rc.position`, v.id)
		if err != nil {
			t.Fatalf("read links for %q: %v", v.cuisine, err)
		}
		var got []string
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				rows.Close()
				t.Fatalf("scan link for %q: %v", v.cuisine, err)
			}
			got = append(got, code)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read links for %q: %v", v.cuisine, err)
		}

		if len(got) != len(v.wantCuisines) {
			t.Errorf("venue %q got cuisines %v, want %v", v.cuisine, got, v.wantCuisines)
			continue
		}
		for i := range got {
			if got[i] != v.wantCuisines[i] {
				t.Errorf("venue %q cuisine[%d] = %q, want %q (full: %v vs %v)",
					v.cuisine, i, got[i], v.wantCuisines[i], got, v.wantCuisines)
			}
		}

		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT cuisine_type FROM restaurants WHERE id = $1`, v.id).Scan(&stored); err != nil {
			t.Fatalf("read cuisine_type for %q: %v", v.cuisine, err)
		}
		if stored != v.cuisine {
			t.Errorf("cuisine_type for %q became %q: the migration must not rewrite the original string", v.cuisine, stored)
		}
	}
}

// TestMigration0079MovesGuestPreferencesToCuisines pins the second half of the
// migration: the foodie profile used to point at restaurant_categories (venue
// TYPES), which made the feed's 400-point cuisine signal unfireable. After the
// migration the column is cuisine_id and it references the real dictionary.
func TestMigration0079MovesGuestPreferencesToCuisines(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()

	var col, refTable string
	err := pool.QueryRow(ctx, `
		SELECT kcu.column_name, ccu.table_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = tc.constraint_name
		WHERE tc.table_name = 'user_cuisine_preferences'
		  AND tc.constraint_type = 'FOREIGN KEY'
		  AND kcu.column_name LIKE '%cuisine%'`).Scan(&col, &refTable)
	if err != nil {
		t.Fatalf("read user_cuisine_preferences foreign keys: %v", err)
	}
	if col != "cuisine_id" || refTable != "cuisines" {
		t.Fatalf("preferences reference %s -> %s, want cuisine_id -> cuisines", col, refTable)
	}

	var stale int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'user_cuisine_preferences' AND column_name = 'category_id'`).Scan(&stale); err != nil {
		t.Fatalf("check for the old column: %v", err)
	}
	if stale != 0 {
		t.Error("user_cuisine_preferences still has category_id: the venue-type link was not removed")
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
