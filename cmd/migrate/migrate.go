package main

import (
	"fmt"
	"os"
)

// Placeholder migration runner. Wire this to goose (pressly/goose/v3) against
// the embedded migrations in internal/../migrations once the DB layer exists:
//
//	goose.RunContext(ctx, command, db, migrations.FS, args...)
//
// Usage: go run ./cmd/migrate/migrate.go [up|down|status]
func main() {
	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	fmt.Printf("migrate: %q not implemented yet — wire goose in cmd/migrate/migrate.go\n", cmd)
}
