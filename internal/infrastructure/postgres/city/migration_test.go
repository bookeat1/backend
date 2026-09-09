package city

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// citiesMigrationVersion is the migration this file exercises. Down/Up runs
// against the SHARED test database, so downThenUp always ends with an Up and
// asserts the database is back at the latest version — a test that left it at
// 80 would fail every other package in the suite.
const citiesMigrationVersion = 81

// citiesMigrationFloor is the version immediately below 0081. The round trip
// reverts down TO it instead of "one step down": goose's Down takes off the
// LAST applied migration, and 0081 stopped being the last one the moment 0082
// landed. From then on the rollback removed a stranger's migration, the 0081
// trigger stayed in place, the raw spellings restored by the mutate hook were
// canonicalized on the spot by that trigger, and the backfill this test exists
// to check was never run at all — a green test measuring nothing (same rot as
// cuisineMigrationFloor / featuresMigrationFloor document).
const citiesMigrationFloor = citiesMigrationVersion - 1

const (
	astanaID = "452c6951-5bde-5a1b-b1b4-8a4c938ae456"
	almatyID = "f157fb6e-7c0a-51d8-9526-37870bc306bf"
)

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
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	// Put the SHARED database back at the latest version whatever happens here,
	// including a t.Fatal between the rollback and the re-apply. Without this a
	// failing assertion in the middle of the down/up dance leaves the database
	// several migrations short and every package that runs afterwards fails on
	// a column missing for reasons of its own.
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("restore the shared test database: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// downThenUp reverts the city migration and applies it again — exactly the
// operation a rollback-and-retry deploy performs.
func downThenUp(t *testing.T, db *sql.DB) {
	t.Helper()
	downMutateUp(t, db, nil)
}

// downMutateUp rolls the migration back, runs mutate while it is rolled back,
// and applies it again.
//
// The hook is what makes the backfill assertions honest. Once the migration is
// applied, its own trigger canonicalizes every write, so a venue seeded from a
// test can never be in the pre-migration state the backfill actually has to
// handle on production. With the migration rolled back the trigger is gone, and
// the raw legacy spellings can be put back exactly as the old system wrote them.
func downMutateUp(t *testing.T, db *sql.DB, mutate func()) {
	t.Helper()
	ctx := context.Background()
	if err := goose.DownToContext(ctx, db, ".", citiesMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", citiesMigrationFloor, err)
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
	if v < citiesMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, citiesMigrationVersion)
	}
}

// seededVenue is one production city spelling under test. wantCity is the code
// the venue must end up linked to ("" = deliberately unlinked), wantString the
// value restaurants.city must hold afterwards.
type seededVenue struct {
	id         uuid.UUID
	city       string
	wantCity   string
	wantString string
}

// TestCityMigrationDownUpAndBackfill checks the three things migration 0081
// promises: it rolls back cleanly, its data transfer is idempotent, and it
// links the venues that already exist without inventing a city for the ones it
// does not recognise.
func TestCityMigrationDownUpAndBackfill(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurants")

	venues := []seededVenue{
		// The two real production spellings (43 + 2 venues on 2026-08-25).
		{uuid.New(), "Алматы", "almaty", "Алматы"},
		{uuid.New(), "Астана", "astana", "Астана"},
		// Case and padding: one city, not three filter values.
		{uuid.New(), "  алматы ", "almaty", "Алматы"},
		{uuid.New(), "АЛМАТЫ", "almaty", "Алматы"},
		// The city's previous official name. Linked AND normalized, because
		// linking alone would leave it invisible to the ?city=Астана filter.
		{uuid.New(), "Нур-Султан", "astana", "Астана"},
		// A city that is genuinely not in the dictionary: no link, and the
		// original string is left exactly as the legacy system wrote it.
		{uuid.New(), "Караганда", "", "Караганда"},
	}
	for _, v := range venues {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category)
			 VALUES ($1,'Venue',$2,'₸₸')`, v.id, v.city); err != nil {
			t.Fatalf("seed venue %q: %v", v.city, err)
		}
	}

	// Put the raw spellings back while the migration is rolled back, so the
	// backfill really runs against pre-migration data.
	restoreRaw := func() {
		for _, v := range venues {
			if _, err := pool.Exec(ctx, `UPDATE restaurants SET city = $2 WHERE id = $1`, v.id, v.city); err != nil {
				t.Fatalf("restore raw city %q: %v", v.city, err)
			}
		}
	}

	db := gooseDB(t)
	downMutateUp(t, db, restoreRaw)

	if got := countRows(t, pool, "cities"); got != 2 {
		t.Fatalf("cities after re-up = %d, want the 2 seeded entries", got)
	}
	assertSeed(t, pool)
	assertVenues(t, pool, venues)

	// Idempotency: another full round trip must not duplicate a single row or
	// change a single id — the ids are uuid v5 of the code precisely so a
	// re-run, and any other environment, produce the same ones.
	beforeAliases := countRows(t, pool, "city_aliases")
	downMutateUp(t, db, restoreRaw)
	if got := countRows(t, pool, "cities"); got != 2 {
		t.Errorf("cities after a second round trip = %d, want 2", got)
	}
	if got := countRows(t, pool, "city_aliases"); got != beforeAliases {
		t.Errorf("city_aliases after a second round trip = %d, want %d", got, beforeAliases)
	}
	assertSeed(t, pool)
	assertVenues(t, pool, venues)
}

// assertSeed pins the two entries a client and a data dump both depend on: the
// fixed ids, the codes, the order the app has always displayed, and the fact
// that the translations are actually there.
func assertSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, code, name, coalesce(name_i18n->>'en',''), display_order, is_active
		   FROM cities ORDER BY display_order`)
	if err != nil {
		t.Fatalf("read cities: %v", err)
	}
	defer rows.Close()

	type row struct {
		id, code, name, en string
		order              int
		active             bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.code, &r.name, &r.en, &r.order, &r.active); err != nil {
			t.Fatalf("scan city: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read cities: %v", err)
	}

	want := []row{
		{astanaID, "astana", "Астана", "Astana", 10, true},
		{almatyID, "almaty", "Алматы", "Almaty", 20, true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cities, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("city[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertVenues checks the migration's central promise about existing data: a
// venue gets the right link, or none at all — and its city STRING is only ever
// canonicalized to a spelling from the dictionary, never invented.
func assertVenues(t *testing.T, pool *pgxpool.Pool, venues []seededVenue) {
	t.Helper()
	ctx := context.Background()
	for _, v := range venues {
		var code *string
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT c.code FROM cities c WHERE c.id = r.city_id), r.city
			   FROM restaurants r WHERE r.id = $1`, v.id).Scan(&code, &stored); err != nil {
			t.Fatalf("read venue %q: %v", v.city, err)
		}
		gotCode := ""
		if code != nil {
			gotCode = *code
		}
		if gotCode != v.wantCity {
			t.Errorf("venue seeded as %q linked to %q, want %q", v.city, gotCode, v.wantCity)
		}
		if stored != v.wantString {
			t.Errorf("venue seeded as %q now stores city %q, want %q", v.city, stored, v.wantString)
		}
	}
}

// TestLegacyInsertStillWorksAndGetsLinked is the compatibility test for the two
// doors the legacy system writes through (legacysync.Sink.UpsertRestaurant and
// cmd/etl): both INSERT a venue with a city STRING and know nothing about the
// dictionary. They must keep working untouched — and end up linked anyway,
// which is why the resolution lives in a database trigger and not in Go.
func TestLegacyInsertStillWorksAndGetsLinked(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurants")

	cases := []struct {
		name, city string
		wantCode   string
		wantString string
	}{
		{"known", "Алматы", "almaty", "Алматы"},
		{"lower case", "астана", "astana", "Астана"},
		{"historical name", "Нур-Султан", "astana", "Астана"},
		{"latin code", "almaty", "almaty", "Алматы"},
		{"unknown", "Караганда", "", "Караганда"},
	}
	for _, c := range cases {
		id := uuid.New()
		// EXACTLY the shape the legacy sink writes: no city_id column at all.
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category)
			 VALUES ($1,$2,$3,'₸₸')`, id, c.name, c.city); err != nil {
			t.Fatalf("legacy-style insert %q: %v", c.city, err)
		}
		var code *string
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT c.code FROM cities c WHERE c.id = r.city_id), r.city
			   FROM restaurants r WHERE r.id = $1`, id).Scan(&code, &stored); err != nil {
			t.Fatalf("read %q: %v", c.name, err)
		}
		got := ""
		if code != nil {
			got = *code
		}
		if got != c.wantCode {
			t.Errorf("%s: linked to %q, want %q", c.name, got, c.wantCode)
		}
		if stored != c.wantString {
			t.Errorf("%s: stored city %q, want %q", c.name, stored, c.wantString)
		}
	}
}

// TestUnknownCityIsRecoverableByNamingTheSpelling is the answer to "what
// happens when the legacy system sends a city we do not have".
//
// The venue is NOT rejected and NOT auto-created as a city: it lands, keeps its
// string, and is simply unlinked — visible as `city_id IS NULL`, which is the
// review queue. Naming the spelling once (an alias, or a new dictionary entry)
// links it on its next write, without a migration and without touching the
// legacy importer.
func TestUnknownCityIsRecoverableByNamingTheSpelling(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurants")

	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category)
		 VALUES ($1,'Venue','Караганда','₸₸')`, id); err != nil {
		t.Fatalf("insert unknown-city venue: %v", err)
	}
	var linked *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT city_id FROM restaurants WHERE id=$1`, id).Scan(&linked); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if linked != nil {
		t.Fatal("an unknown city was linked to something; the migration must not guess")
	}

	repo := New(pool)
	karaganda := uuid.New()
	entry := domain.CityEntry{ID: karaganda, Code: "karaganda", Name: "Караганда", DisplayOrder: 30, IsActive: true}
	if err := repo.Create(ctx, &entry); err != nil {
		t.Fatalf("create the missing city: %v", err)
	}
	// The venue is linked by its next write — no backfill job, no redeploy.
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET updated_at = now() WHERE id=$1`, id); err != nil {
		t.Fatalf("touch venue: %v", err)
	}
	var code string
	if err := pool.QueryRow(ctx,
		`SELECT c.code FROM restaurants r JOIN cities c ON c.id = r.city_id WHERE r.id=$1`,
		id).Scan(&code); err != nil {
		t.Fatalf("venue was not linked after the city appeared: %v", err)
	}
	if code != "karaganda" {
		t.Errorf("linked to %q, want karaganda", code)
	}

	// Clean up so the shared database keeps the dictionary the migration seeds.
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET city_id = NULL WHERE id=$1`, id); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cities WHERE id=$1`, karaganda); err != nil {
		t.Fatalf("cleanup city: %v", err)
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
