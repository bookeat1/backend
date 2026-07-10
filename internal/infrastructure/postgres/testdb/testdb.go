// Package testdb provides a Postgres connection for integration tests. Tests
// skip when -short is set or TEST_DATABASE_URL is empty, so `go test -short`
// stays hermetic. TEST_DATABASE_URL must point at a migrated database.
package testdb

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Connect returns a live *sql.DB or skips the test.
func Connect(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { db.Close() })
	return db
}

// Truncate clears the given tables between tests.
func Truncate(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	for _, tbl := range tables {
		if _, err := db.Exec("TRUNCATE " + tbl + " CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}
