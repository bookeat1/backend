.PHONY: run build tidy migrate-up migrate-down test test-short fmt vet check etl test-integration

run:
	go run ./cmd/http/main.go

build:
	go build ./...

tidy:
	go mod tidy

migrate-up:
	go run ./cmd/migrate/migrate.go up

migrate-down:
	go run ./cmd/migrate/migrate.go down

# Full suite — may require Docker for integration/functional tests.
test:
	go test ./...

# Unit-only — skips anything needing external services.
test-short:
	go test -short ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet
	go build ./...

etl:
	go run ./cmd/etl/main.go

# Integration tests need a migrated Postgres; point TEST_DATABASE_URL at it.
test-integration:
	go test ./...
