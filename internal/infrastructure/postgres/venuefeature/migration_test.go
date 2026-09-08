package venuefeature

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

// featuresMigrationVersion is the migration this file exercises. Down/Up runs
// against the SHARED test database, so downThenUp always ends with an Up and
// asserts the database is back at the latest version — a test that left it at
// 81 would fail every other package in the suite.
const featuresMigrationVersion = 82

// seededFeatures is the number of dictionary entries migration 0082 seeds.
const seededFeatures = 19

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

// featuresMigrationFloor is the version just below 0082. Reverting down TO a
// version rather than "one step down" keeps this test honest once anything is
// stacked on top: a step count silently starts reverting the WRONG migration
// and the test goes on passing while exercising nothing (that is exactly how
// the cuisine round-trip test rotted when 0081 landed).
const featuresMigrationFloor = 81

// downThenUp reverts migration 0082 and applies it again — exactly what a
// rollback-and-retry deploy performs.
func downThenUp(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if err := goose.DownToContext(ctx, db, ".", featuresMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", featuresMigrationFloor, err)
	}
	// While the migration is rolled back, the free-text table it replaced must
	// be BACK — a Down that leaves the old schema missing is not a rollback,
	// it is a one-way door with a rollback-shaped label on it.
	if !tableExists(t, db, "restaurant_features") {
		t.Fatal("after Down the free-text restaurant_features table is missing: the rollback is not reversible")
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if v < featuresMigrationVersion {
		t.Fatalf("database left at version %d, want >= %d", v, featuresMigrationVersion)
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return n > 0
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// legacyVenue is one venue from the OLD system, seeded with its real id, plus
// the feature codes the owner approved for it on 2026-08-25.
type legacyVenue struct {
	id       uuid.UUID
	name     string
	wantFeat []string
}

// prodLegacyVenues are the eight venues that end up with features, with the
// exact ids they have in BOTH systems (checked against production 2026-08-25).
// The two remaining venues from the legacy dump (Chaihana Palau, INZHU) are
// listed with an empty set on purpose: everything they had recorded as a
// "feature" was a cuisine or marketing copy, and the approved outcome is that
// they get nothing rather than something plausible.
var prodLegacyVenues = []legacyVenue{
	{uuid.MustParse("21d70e1c-4a0d-43ee-b40e-e2205f4ba310"), "1100 Karaoke", []string{"terrace", "vip_rooms"}},
	{uuid.MustParse("bdce796d-bf6a-41fa-b1ad-5076aa1ede38"), "Abay", []string{"view"}},
	{uuid.MustParse("937b075f-c8b0-43e7-97a7-4d8f45c2b96a"), "Aiza Esentai", []string{"wifi", "terrace", "business_lunch"}},
	{uuid.MustParse("660fd375-ac57-4c0b-b461-9424cc3133d9"), "Aiza Miras", []string{"wifi", "breakfast", "takeaway", "vegetarian_menu"}},
	{uuid.MustParse("2de7a222-8b5f-4926-b831-c5da789fb711"), "Guinness Pub", []string{"live_music", "sports_broadcasts"}},
	{uuid.MustParse("4282dd37-5b49-4a4d-8c3c-79f4423a5e7e"), "Hooqa Room", []string{"hookah", "live_music"}},
	{uuid.MustParse("9f7ce1c4-606a-49f7-a17e-a22d20ea157d"), "Koktobe Terrace", []string{"terrace", "view"}},
	{uuid.MustParse("653782ce-5ed7-4e75-9575-bb2165368ecb"), "Mongol Bar Мирас", []string{"terrace", "wifi", "wine_list"}},
	{uuid.MustParse("d2f0e053-61b9-407a-8816-ceb370d65d22"), "Chaihana Palau", nil},
	{uuid.MustParse("e8877753-fcbc-4470-a66d-6b47b3f218e4"), "INZHU", nil},
}

// TestMigration0082SeedsDictionaryAndApprovedLinks checks everything the
// migration promises at once: the dictionary is seeded, the owner-approved
// per-venue layout lands exactly (no more, no less), the free-text table is
// gone, the whole thing rolls back, and a second round trip changes nothing.
func TestMigration0082SeedsDictionaryAndApprovedLinks(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurant_venue_features", "restaurants")

	for _, v := range prodLegacyVenues {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category, is_active)
			 VALUES ($1,$2,'Алматы','₸₸',true)`, v.id, v.name); err != nil {
			t.Fatalf("seed venue %s: %v", v.name, err)
		}
	}

	db := gooseDB(t)
	downThenUp(t, db)

	if got := countRows(t, pool, "venue_features"); got != seededFeatures {
		t.Fatalf("venue_features after re-up = %d, want %d seeded entries", got, seededFeatures)
	}
	if tableExists(t, db, "restaurant_features") {
		t.Error("the free-text restaurant_features table is still there after Up: the migration must drop it")
	}
	assertVenueFeatures(t, pool)

	// Idempotency: a second round trip must not duplicate a single row.
	beforeLinks := countRows(t, pool, "restaurant_venue_features")
	beforeAliases := countRows(t, pool, "venue_feature_aliases")
	downThenUp(t, db)
	if got := countRows(t, pool, "venue_features"); got != seededFeatures {
		t.Errorf("venue_features after a second round trip = %d, want %d", got, seededFeatures)
	}
	if got := countRows(t, pool, "restaurant_venue_features"); got != beforeLinks {
		t.Errorf("links after a second round trip = %d, want %d", got, beforeLinks)
	}
	if got := countRows(t, pool, "venue_feature_aliases"); got != beforeAliases {
		t.Errorf("aliases after a second round trip = %d, want %d", got, beforeAliases)
	}
	assertVenueFeatures(t, pool)
}

func assertVenueFeatures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, v := range prodLegacyVenues {
		rows, err := pool.Query(ctx,
			`SELECT f.code FROM restaurant_venue_features rvf
			   JOIN venue_features f ON f.id = rvf.feature_id
			  WHERE rvf.restaurant_id = $1
			  ORDER BY rvf.position`, v.id)
		if err != nil {
			t.Fatalf("read links for %s: %v", v.name, err)
		}
		var got []string
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				rows.Close()
				t.Fatalf("scan link for %s: %v", v.name, err)
			}
			got = append(got, code)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read links for %s: %v", v.name, err)
		}
		if len(got) != len(v.wantFeat) {
			t.Errorf("venue %s got features %v, want %v", v.name, got, v.wantFeat)
			continue
		}
		for i := range got {
			if got[i] != v.wantFeat[i] {
				t.Errorf("venue %s feature[%d] = %q, want %q (full: %v vs %v)",
					v.name, i, got[i], v.wantFeat[i], got, v.wantFeat)
			}
		}
	}
}

// TestMigration0082DoesNotInventAliases pins the negative half of the approved
// layout. The alias table IS the layout: anything in it will silently attach a
// venue to a feature forever after. So the values the owner ruled OUT must not
// be in there — and «постное меню» specifically must not, even though its venue
// did get the vegetarian feature: that link was made explicitly, exactly
// because the two words are not synonyms (постное allows fish).
func TestMigration0082DoesNotInventAliases(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()

	rejected := []string{
		"восточная кухня", "национальная кухня", // cuisines
		"рыбная витрина", "устричная витрина", // cuisines again
		"коктобе",                                   // a district
		"колоритный интерьер", "необычный интерьер", // marketing
		"профессиональный звук",          // a spec for whoever rents the hall
		"киновечера", "чайные церемонии", // events
		"постное меню", // NOT a synonym of «вегетарианское меню»
	}
	for _, alias := range rejected {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM venue_feature_aliases WHERE alias = $1`, alias).Scan(&n); err != nil {
			t.Fatalf("check alias %q: %v", alias, err)
		}
		if n != 0 {
			t.Errorf("alias %q exists: the owner ruled this value out, and an alias would re-attach it forever", alias)
		}
	}

	// The approved ones ARE there, or the transfer above passed by accident.
	for _, alias := range []string{"lounge", "вип кабинки", "панорамный вид на горы", "вид на предгорные пейзажи"} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM venue_feature_aliases WHERE alias = $1`, alias).Scan(&n); err != nil {
			t.Fatalf("check alias %q: %v", alias, err)
		}
		if n != 1 {
			t.Errorf("approved alias %q is missing", alias)
		}
	}
}
