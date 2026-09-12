package promocode

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/migrations"
)

// codeMigrationVersion is the migration this file exercises.
const codeMigrationVersion = 108

// codeMigrationFloor is the version immediately below it. Rolling back "one
// step" only means 0108 for as long as 0108 IS the last applied migration —
// the moment a later one lands, a plain Down would roll back a STRANGER's
// migration and this test would measure nothing. Roll down TO the floor
// explicitly instead (the platformpages/appversion precedent).
const codeMigrationFloor = codeMigrationVersion - 1

// gooseDB opens goose's database/sql handle onto the same TEST_DATABASE_URL
// the pgx pool uses and guarantees the SHARED database is put back at the
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

// TestMigration0108RollsBackAndReapplies runs what a rollback-and-retry deploy
// performs, on tables that ALREADY HAVE LIVE ROWS — a code somebody created
// in the cabinet and a booking taken with it. Three properties matter:
//
//  1. the DOWN succeeds with rows present (a booking referencing a code does
//     NOT block dropping the table — promo_code_id deliberately carries no
//     foreign key);
//  2. the rollback keeps the booking itself and its promotion_id, so a guest
//     who took part in the campaign stays a participant even though "came via
//     a code" is lost;
//  3. the re-applied UP brings back the table, the columns and — the part
//     ADR-047 depends on — the partial index the activation counts run over.
func TestMigration0108RollsBackAndReapplies(t *testing.T) {
	pool := testdb.Connect(t)
	db := gooseDB(t)
	ctx := context.Background()
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	user := seedUser(ctx, t, pool, 1)
	code := mkCode(pid, "MARATHON26")
	if err := New(pool).Create(ctx, code); err != nil {
		t.Fatalf("seed promo code: %v", err)
	}

	start := time.Now().Add(24 * time.Hour)
	bookingID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, user_id, name, phone, phone_normalized,
			guests, starts_at, ends_at, promotion_id, promo_code_id, promo_code)
		 VALUES ($1, $2, $3, 'Guest', '+7700', '+77000000001', 2, $4, $5, $6, $7, $8)`,
		bookingID, rid, user, start, start.Add(2*time.Hour), pid, code.ID, code.Code); err != nil {
		t.Fatalf("seed booking with the code: %v", err)
	}

	if err := goose.DownToContext(ctx, db, ".", codeMigrationFloor); err != nil {
		t.Fatalf("goose down to %d: %v", codeMigrationFloor, err)
	}

	var tableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.promo_codes') IS NOT NULL`).Scan(&tableExists); err != nil {
		t.Fatalf("check promo_codes after down: %v", err)
	}
	if tableExists {
		t.Error("the rollback left promo_codes behind")
	}

	var leftoverCols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'bookings' AND column_name IN ('promo_code_id', 'promo_code')`).
		Scan(&leftoverCols); err != nil {
		t.Fatalf("check booking columns after down: %v", err)
	}
	if leftoverCols != 0 {
		t.Errorf("the rollback left %d promo code column(s) on bookings", leftoverCols)
	}

	// The booking survives the rollback, promotion_id and all: participation
	// in the campaign is not lost, only the "came via a code" detail is.
	var survivedPromotion *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT promotion_id FROM bookings WHERE id = $1`, bookingID).Scan(&survivedPromotion); err != nil {
		t.Fatalf("read the booking after down: %v", err)
	}
	if survivedPromotion == nil || *survivedPromotion != pid {
		t.Errorf("the rollback lost the booking's promotion_id: %v", survivedPromotion)
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < codeMigrationVersion {
		t.Fatalf("the database came back at version %d, below %d", version, codeMigrationVersion)
	}

	// The columns are back and the re-applied schema is writable end to end.
	code2 := mkCode(pid, "MARATHON26")
	if err := New(pool).Create(ctx, code2); err != nil {
		t.Fatalf("create a code after the round trip: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE bookings SET promo_code_id = $2, promo_code = $3 WHERE id = $1`,
		bookingID, code2.ID, code2.Code); err != nil {
		t.Fatalf("write the booking's promo columns after the round trip: %v", err)
	}

	// ADR-047's mandatory index: without it both activation counts scan the
	// whole bookings table, which only shows up as a production slowdown.
	var partial bool
	if err := pool.QueryRow(ctx,
		`SELECT indexdef LIKE '%promo_code_id%' AND indexdef LIKE '%WHERE%'
		   FROM pg_indexes WHERE indexname = 'idx_bookings_promo_code'`).Scan(&partial); err != nil {
		t.Fatalf("the partial index on bookings(promo_code_id) is missing after the round trip: %v", err)
	}
	if !partial {
		t.Error("idx_bookings_promo_code came back without its WHERE clause")
	}
}

// TestMigration0108Constraints proves the schema itself refuses what
// domain.PromoCode.Validate refuses, so a row written around the application
// (psql, a future import) cannot break the feature's assumptions. The
// application-side mapping of these failures is covered in repository_test.go;
// here the point is that the CONSTRAINTS exist at all.
func TestMigration0108Constraints(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			"code must be normalized",
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at)
			 VALUES ($1, 'marathon 26', $2, now() + interval '1 day')`,
			[]any{uuid.New(), pid},
		},
		{
			"window must be non-empty",
			`INSERT INTO promo_codes (id, code, promotion_id, starts_at, expires_at)
			 VALUES ($1, 'WINDOW26', $2, now(), now() - interval '1 day')`,
			[]any{uuid.New(), pid},
		},
		{
			"total limit may not be zero",
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at, max_uses_total)
			 VALUES ($1, 'ZERO26', $2, now() + interval '1 day', 0)`,
			[]any{uuid.New(), pid},
		},
		{
			"per-user limit may not be zero",
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at, max_uses_per_user)
			 VALUES ($1, 'PERUSER26', $2, now() + interval '1 day', 0)`,
			[]any{uuid.New(), pid},
		},
		{
			"status is limited to the four known values",
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at, status)
			 VALUES ($1, 'STATUS26', $2, now() + interval '1 day', 'published')`,
			[]any{uuid.New(), pid},
		},
		{
			"the promo behind a code must exist",
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at)
			 VALUES ($1, 'ORPHAN26', $2, now() + interval '1 day')`,
			[]any{uuid.New(), uuid.New()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tt.sql, tt.args...); err == nil {
				t.Fatal("the database accepted a row it must refuse")
			}
		})
	}

	t.Run("defaults are draft, one use per user, unlimited total", func(t *testing.T) {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO promo_codes (id, code, promotion_id, expires_at)
			 VALUES ($1, 'DEFAULTS26', $2, now() + interval '1 day')`, id, pid); err != nil {
			t.Fatalf("insert with defaults: %v", err)
		}
		got, err := New(pool).GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		// A freshly inserted code must never be redeemable by accident: the
		// default status is draft, not active.
		if got.Status != domain.PromoCodeDraft {
			t.Errorf("default status = %q, want draft", got.Status)
		}
		if got.MaxUsesPerUser != 1 {
			t.Errorf("default max_uses_per_user = %d, want 1", got.MaxUsesPerUser)
		}
		if got.MaxUsesTotal != nil {
			t.Errorf("default max_uses_total = %d, want NULL (unlimited)", *got.MaxUsesTotal)
		}
		if got.CreatedBy != nil {
			t.Errorf("default created_by = %v, want NULL", got.CreatedBy)
		}
	})
}

// TestCreatedByCarriesNoForeignKey is the TEST-DB POLLUTION TRAP guard
// (conventions/bookeat-backend.md): promo_codes.created_by must stay a plain
// uuid. `TRUNCATE ... CASCADE` — what dozens of packages run as
// `testdb.Truncate(t, pool, ..., "users")` — clears every table with an FK on
// the truncated one and ignores ON DELETE SET NULL entirely, so an
// attribution column with a real FK would be a second, avoidable way for this
// table to vanish mid-suite.
//
// It is only the SECOND way, not the first: promo_codes must reference promos
// (that is what a code means), promos.feed_reviewed_by already references
// users, and TRUNCATE CASCADE follows that chain transitively — a
// users-truncate takes promos and therefore promo_codes with it whatever this
// table declares. That is measured below and is exactly why no test may treat
// promo_codes as a seeded dictionary: every test creates its own codes, and
// the marathon code in production comes from the cabinet, not from a seed.
func TestCreatedByCarriesNoForeignKey(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	var fks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conrelid = 'promo_codes'::regclass AND contype = 'f'
		    AND confrelid = 'users'::regclass`).Scan(&fks); err != nil {
		t.Fatalf("read promo_codes foreign keys: %v", err)
	}
	if fks != 0 {
		t.Fatalf("promo_codes declares %d foreign key(s) on users; created_by must stay a plain uuid", fks)
	}

	// The attribution survives the user row itself disappearing — this is the
	// behaviour the admin listing has to tolerate and the reason the column
	// needs no FK to be useful.
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	author := seedUser(ctx, t, pool, 1)
	code := mkCode(pid, "MARATHON26")
	code.CreatedBy = &author
	if err := New(pool).Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, author); err != nil {
		t.Fatalf("delete the author: %v", err)
	}
	got, err := New(pool).GetByID(ctx, code.ID)
	if err != nil {
		t.Fatalf("the code did not survive its author being deleted: %v", err)
	}
	if got.CreatedBy == nil || *got.CreatedBy != author {
		t.Errorf("CreatedBy = %v, want the original author %v", got.CreatedBy, author)
	}

	// And the transitive wipe, stated as a fact so nobody later builds a
	// seeded fixture on this table: a users-truncate reaches promo_codes
	// through promos.feed_reviewed_by.
	testdb.Truncate(t, pool, "users")
	if _, err := New(pool).GetByID(ctx, code.ID); err == nil {
		t.Log("note: promo_codes now survives TRUNCATE users CASCADE — the chain " +
			"through promos.feed_reviewed_by must have changed; the comment above needs updating")
	}
}
