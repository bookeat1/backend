.PHONY: run build tidy migrate-up migrate-down test test-short mocks fmt vet check

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

# Regenerate mockgen mocks. Add one line per mocked interface as the domain grows.
mocks:
	@echo "no mocks defined yet — add mockgen lines here as interfaces appear"

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet
	go build ./...
