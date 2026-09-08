package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"backend-core/internal/logger"
)

// stagingDDL is a MINIMAL stand-in for the raw_supabase dump: only the tables
// and columns runRestaurants actually reads. It is not a copy of the real dump
// schema and is not meant to validate it — if it drifts, the step fails loudly
// with an SQL error rather than passing quietly.
const stagingDDL = `
CREATE SCHEMA IF NOT EXISTS raw_supabase;
CREATE TABLE raw_supabase.restaurant_categories (
  id uuid PRIMARY KEY, name varchar, name_i18n jsonb, description varchar,
  description_i18n jsonb, created_at timestamptz);
CREATE TABLE raw_supabase.restaurants (
  id uuid PRIMARY KEY, category_id uuid, name varchar, name_i18n jsonb,
  description varchar, description_i18n jsonb, cuisine_type varchar,
  cuisine_type_i18n jsonb, address varchar, address_i18n jsonb,
  opening_hours varchar, opening_hours_i18n jsonb, city varchar,
  price_category varchar, email varchar, phone varchar,
  latitude double precision, longitude double precision,
  kwaaka_restaurant_id varchar, is_active boolean, is_new boolean,
  is_popular boolean, is_premium boolean, hidden_from_home boolean,
  display_order integer, created_at timestamptz, updated_at timestamptz);
CREATE TABLE raw_supabase.restaurant_features (
  id uuid PRIMARY KEY, restaurant_id uuid, name varchar, name_i18n jsonb, created_at timestamptz);
CREATE TABLE raw_supabase.restaurant_images (
  id uuid PRIMARY KEY, restaurant_id uuid, image_url varchar, is_primary boolean, created_at timestamptz);
CREATE TABLE raw_supabase.restaurant_tags (
  id uuid PRIMARY KEY, restaurant_id uuid, tag_name varchar, tag_name_i18n jsonb, created_at timestamptz);
CREATE TABLE raw_supabase.restaurant_social_links (
  id uuid PRIMARY KEY, restaurant_id uuid, type varchar, url varchar, created_at timestamptz);
CREATE TABLE raw_supabase.restaurant_working_hours (
  id uuid PRIMARY KEY, restaurant_id uuid, day_of_week integer, open_time varchar,
  close_time varchar, is_open boolean, created_at timestamptz, updated_at timestamptz);
CREATE TABLE raw_supabase.restaurant_time_slots (
  id uuid PRIMARY KEY, restaurant_id uuid, day_of_week integer, start_time varchar,
  end_time varchar, is_manually_disabled boolean, created_at timestamptz, updated_at timestamptz);
CREATE TABLE raw_supabase.restaurant_tables (
  id uuid PRIMARY KEY, restaurant_id uuid, name varchar, capacity integer,
  description varchar, is_active boolean, created_at timestamptz, updated_at timestamptz);
CREATE TABLE raw_supabase.restaurant_floor_plans (
  id uuid PRIMARY KEY, restaurant_id uuid, layout_data jsonb,
  created_at timestamptz, updated_at timestamptz);
CREATE TABLE raw_supabase.restaurant_managers (
  id uuid PRIMARY KEY, restaurant_id uuid, user_id uuid, created_by uuid,
  whatsapp_opt_in boolean, whatsapp_phone varchar, created_at timestamptz);
CREATE TABLE raw_supabase.restaurant_partnership_requests (
  id uuid PRIMARY KEY, restaurant_name varchar, contact_name varchar, email varchar,
  phone varchar, address varchar, cuisine_type varchar, description varchar,
  additional_info varchar, status varchar, created_at timestamptz, updated_at timestamptz);
`

// etlDB connects to the same migrated database the other integration tests use,
// builds the staging schema and drops it again afterwards.
func etlDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP SCHEMA IF EXISTS raw_supabase CASCADE`); err != nil {
			t.Errorf("drop staging schema: %v", err)
		}
		if _, err := db.Exec(`TRUNCATE restaurants CASCADE`); err != nil {
			t.Errorf("truncate restaurants: %v", err)
		}
		db.Close()
	})
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS raw_supabase CASCADE`); err != nil {
		t.Fatalf("reset staging schema: %v", err)
	}
	if _, err := db.Exec(stagingDDL); err != nil {
		t.Fatalf("create staging schema: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE restaurants CASCADE`); err != nil {
		t.Fatalf("truncate restaurants: %v", err)
	}
	return db
}

const etlVenue = "cccccccc-0000-0000-0000-000000000001"

// TestETLDoesNotOverwriteCuisineAndCity guards the OTHER door into the
// restaurants table. legacysync is the automatic one; this importer is run by
// hand and is exactly the kind of thing that gets started months later "just to
// refresh the catalog" — with the old assignment list it would have quietly
// reverted every cuisine and every city to the values in the dump.
//
// As in the legacysync guard, the test also asserts that the columns the dump
// still owns DID change: without that half it would pass just as happily if the
// importer stopped updating anything at all.
func TestETLDoesNotOverwriteCuisineAndCity(t *testing.T) {
	db := etlDB(t)
	ctx := context.Background()
	log := logger.New("error", "text")

	if _, err := db.ExecContext(ctx, `
INSERT INTO raw_supabase.restaurants
 (id, name, description, cuisine_type, address, opening_hours, city, price_category,
  email, phone, is_active, created_at, updated_at)
VALUES ($1,'Rest One','d','italian','addr','10-22','Алматы','₸₸','r@x.kz','+7700',true, now(), now())`,
		etlVenue); err != nil {
		t.Fatalf("seed staging venue: %v", err)
	}

	// First import: the dump's values land, because we have nothing better yet.
	if err := runRestaurants(ctx, db, log); err != nil {
		t.Fatalf("first import: %v", err)
	}
	var cuisine, city string
	if err := db.QueryRowContext(ctx,
		`SELECT cuisine_type, city FROM restaurants WHERE id = $1`, etlVenue).Scan(&cuisine, &city); err != nil {
		t.Fatalf("read after first import: %v", err)
	}
	if cuisine != "italian" || city != "Алматы" {
		t.Fatalf("after first import cuisine=%q city=%q, want the dump's values on INSERT", cuisine, city)
	}

	// Now WE decide both, in the panel.
	if _, err := db.ExecContext(ctx,
		`UPDATE restaurants SET cuisine_type = $2, cuisine_type_i18n = $3, city = $4 WHERE id = $1`,
		etlVenue, "Итальянская, Европейская", []byte(`{"en":"Italian, European"}`), "Астана"); err != nil {
		t.Fatalf("set our cuisine and city: %v", err)
	}

	// The dump is refreshed: name and address change, cuisine and city stay
	// stale — and someone runs the importer by hand.
	if _, err := db.ExecContext(ctx,
		`UPDATE raw_supabase.restaurants SET name = 'Rest One Renamed', address = 'Dump Address 2' WHERE id = $1`,
		etlVenue); err != nil {
		t.Fatalf("update staging venue: %v", err)
	}
	if err := runRestaurants(ctx, db, log); err != nil {
		t.Fatalf("second import: %v", err)
	}

	var name, address string
	var i18n []byte
	if err := db.QueryRowContext(ctx,
		`SELECT name, address, cuisine_type, cuisine_type_i18n, city FROM restaurants WHERE id = $1`,
		etlVenue).Scan(&name, &address, &cuisine, &i18n, &city); err != nil {
		t.Fatalf("read after second import: %v", err)
	}
	// The columns the dump still owns DID update — otherwise this test would
	// also pass if the importer had simply stopped writing.
	if name != "Rest One Renamed" {
		t.Errorf("name = %q, want the dump's update to still apply", name)
	}
	if address != "Dump Address 2" {
		t.Errorf("address = %q, want the dump's update to still apply", address)
	}
	if cuisine != "Итальянская, Европейская" {
		t.Errorf("cuisine_type = %q, want OUR value untouched by the importer", cuisine)
	}
	if string(i18n) != `{"en": "Italian, European"}` && string(i18n) != `{"en":"Italian, European"}` {
		t.Errorf("cuisine_type_i18n = %s, want OUR value untouched by the importer", i18n)
	}
	if city != "Астана" {
		t.Errorf("city = %q, want OUR value untouched by the importer", city)
	}
}

// TestETLDoesNotImportFreeTextFeatures guards the THIRD thing this importer used
// to own. Until migration 0082 it copied raw_supabase.restaurant_features
// verbatim — the table where the old system kept a cuisine («Восточная кухня»),
// a district («Коктобе») and a sound-engineering spec under the same column as
// «Wi-Fi».
//
// Two failures are possible and both are covered by one run: the step could
// still exist and blow up on the table that no longer exists (the importer must
// finish cleanly), or someone could recreate the table and have the step
// quietly re-import the mess on top of the approved dictionary links (the
// venue's feature set must be exactly what we assigned).
func TestETLDoesNotImportFreeTextFeatures(t *testing.T) {
	db := etlDB(t)
	ctx := context.Background()
	log := logger.New("error", "text")

	if _, err := db.ExecContext(ctx, `
INSERT INTO raw_supabase.restaurants
 (id, name, description, cuisine_type, address, opening_hours, city, price_category,
  email, phone, is_active, created_at, updated_at)
VALUES ($1,'Rest Two','d','italian','addr','10-22','Алматы','₸₸','r@x.kz','+7700',true, now(), now())`,
		etlVenue); err != nil {
		t.Fatalf("seed staging venue: %v", err)
	}
	// The dump still carries its free-text "features" — that is the point.
	if _, err := db.ExecContext(ctx, `
INSERT INTO raw_supabase.restaurant_features (id, restaurant_id, name, created_at)
VALUES (gen_random_uuid(), $1, 'Восточная кухня', now()),
       (gen_random_uuid(), $1, 'Коктобе', now())`, etlVenue); err != nil {
		t.Fatalf("seed staging features: %v", err)
	}

	if err := runRestaurants(ctx, db, log); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// WE assign a feature, the way the panel will.
	if _, err := db.ExecContext(ctx, `
INSERT INTO restaurant_venue_features (restaurant_id, feature_id, position)
SELECT $1, id, 0 FROM venue_features WHERE code = 'wifi'`, etlVenue); err != nil {
		t.Fatalf("assign our feature: %v", err)
	}

	// Someone runs the importer again by hand, months later.
	if err := runRestaurants(ctx, db, log); err != nil {
		t.Fatalf("second import: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
SELECT f.code FROM restaurant_venue_features rvf
  JOIN venue_features f ON f.id = rvf.feature_id
 WHERE rvf.restaurant_id = $1 ORDER BY rvf.position`, etlVenue)
	if err != nil {
		t.Fatalf("read features: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read features: %v", err)
	}
	if len(got) != 1 || got[0] != "wifi" {
		t.Errorf("features after the importer ran = %v, want exactly [wifi] — ours, untouched", got)
	}
}
