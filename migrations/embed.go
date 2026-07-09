// Package migrations embeds the goose SQL migration files so they ship inside
// the binary. Add migrations as NNNN_description.sql in goose format:
//
//	-- +goose Up
//	CREATE TABLE ...;
//	-- +goose Down
//	DROP TABLE ...;
//
// Use VARCHAR for enumerated fields (validated in app code) — never CREATE TYPE
// ... AS ENUM.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
