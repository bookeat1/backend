.PHONY: run build tidy migrate-up migrate-down test test-short fmt vet check etl test-integration swagger

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

# Regenerate the committed OpenAPI/Swagger spec from swaggo annotations.
# Runs swag as a one-off tool (no go.mod dependency); docs.go is discarded so
# nothing pulls swaggo into the build.
swagger:
	go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/http/swagger.go -d ./ --parseInternal -o docs
	rm -f docs/docs.go
