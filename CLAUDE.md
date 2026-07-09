# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`backend-core` is the core backend service of **BookEat**. It is a fresh Go service scaffolded with a Clean/Hexagonal architecture. The domain is still being defined — treat the sections below as the **authoritative rules for how code must be structured**, not as a description of already-existing entities.

This is a **public** project. Do **not** add any private/internal dependencies (no private module registries, no company-internal libraries). Everything must be buildable from public modules and the Go standard library.

## Commands

```bash
make run            # go run ./cmd/http/main.go   — HTTP server
make build          # go build ./...
make tidy           # go mod tidy

make migrate-up     # apply migrations (goose)
make migrate-down   # roll back last migration
go run ./cmd/migrate/migrate.go status

make test           # go test ./...       — full suite
make test-short     # go test -short ./... — unit only
go test ./internal/usecase/<pkg>/ -run TestName   # single test

make mocks          # regenerate mockgen mocks (run after changing any mocked interface)

go vet ./... && gofmt -w .
```

Config is **fully environment-variable based** — there is no config file. All entry points load it via **`bootstrap.NewConfig()`** (`internal/bootstrap/config.go`), which reads env vars with sane defaults and auto-loads a local `.env` when present (real env vars win over `.env`). Copy `.env.example` → `.env` for local development; never commit `.env`. Add new settings as typed fields on `Config` plus a `getEnv*` call in `NewConfig`.

## Architecture

Clean/Hexagonal. Dependencies point **inward**: `transport → usecase → domain ← infrastructure`. The `domain` package is the center and imports nothing from the outer layers (and no frameworks). Wiring happens in **`internal/bootstrap/deps.go`** (`NewDeps`) — the single place where concrete repos, integration clients, usecases, and handlers are constructed and connected. Read it first to understand how anything is assembled.

**Entry points** (`cmd/`): `http/main.go` is the HTTP server. `migrate/migrate.go` runs goose migrations. Add new entry points as thin `main` wrappers that call `bootstrap.NewConfig()` then into `internal/bootstrap`.

**`internal/domain/`** — flat package, **one file per entity** (`user.go`, `<entity>.go`, …). Each file holds the entity struct, its repository interface, and related typed constants. **No business logic, no frameworks** (no gin/sql/http imports). Status/role/state values are Go constants of a named string type (e.g. `type Role string`), and are stored as `VARCHAR` in the DB — **never `CREATE TYPE ... AS ENUM`** in migrations. `errors.go` defines the sentinel errors (`ErrNotFound`, `ErrAlreadyExists`, `ErrForbidden`, `ErrUnauthorized`, `ErrInvalidStatus`, `ErrValidation`) that drive HTTP status mapping. `tx.go` defines the `TxManager` port.

**`internal/usecase/`** — application logic, grouped by actor/context (e.g. `admin/`, `users/`, …). A `Facade` holds basic CRUD/read operations; complex operations get their own file + interface next to the facade (e.g. `<pkg>/<operation>.go` defining a focused `...UseCase` interface). A usecase **never imports another domain's concrete repository** — when it needs cross-domain data or an external system it declares a minimal local **port interface** (Interface Segregation), bound to a concrete impl in `deps.go`. Ports for transactions and external systems live in `domain` (`domain.TxManager`, …).

**`internal/transport/rest/`** — Gin HTTP handlers, grouped like usecases, plus shared `middleware/`, `response/`, `httputil/`. Per resource:
- `handler.go` — depends on the usecase facade/interfaces, exposes `RegisterRoutes`.
- `request.go` — input DTOs with `validate:"..."` tags, a `Validate()` method, and a `ToDomain()` mapper.
- `response.go` — output DTOs with a `fromDomain()` mapper.

Handlers wrap **all** responses in `response.Envelope` (`OK`/`Created`/`Error`) and route **every** error through **`response.HandleError`**, which maps domain sentinels to status codes (404/409/403/401/422/500). Always `return` immediately after writing an error response.

**`internal/infrastructure/`** — implements domain interfaces; depends only on `domain`. `postgres/<entity>/repository.go` per entity; external-service HTTP clients live in their own subpackage; `sqltx/` provides the transaction manager.

**`internal/logger/`** — thin logging wrapper over the standard library `log/slog`. Do **not** pull in a private logging library.

### Transactions

`domain.TxManager` (`WithinTx(ctx, fn)`) is implemented by `sqltx.Manager`. It injects the active `*sql.Tx` into the context; nested `WithinTx` calls reuse the existing tx (no double-begin). Postgres repositories must pull the active tx from the context via `sqltx.From(ctx)` so that multi-repository operations inside a usecase share one transaction.

### Auth & routing (`bootstrap/app.go`)

- `/health` is **unauthenticated**.
- `/api/*` runs `middleware.Auth`: strips the `Bearer` token, verifies it, loads the local user, rejects inactive users, and stashes an `AuthUser{ID, Role}` in the request context (read via `middleware.GetAuthUser`).
- Role-restricted groups use `middleware.RequireRole`.

## Conventions

- **Errors:** return domain sentinel errors from usecases/repositories; never leak SQL or transport errors upward. Wrap with `fmt.Errorf("...: %w", err)` to preserve the sentinel for `errors.Is`.
- **Migrations:** SQL in `migrations/`, goose format (`-- +goose Up` / `-- +goose Down`), embedded via `migrations/embed.go`. `VARCHAR` for enumerated fields, validated in app code — no DB enums.
- **Naming:** package names are short, lower-case, no underscores. Interfaces are declared where they are **consumed** (the usecase/transport layer), not where implemented.
- **Formatting:** run `gofmt -w .` and `go vet ./...` before finishing any change.
- **Mocks:** after changing any interface listed in the `mocks` target of the Makefile, run `make mocks`. Generated mocks live under `internal/mocks/`.
- **Tests:** table-driven where it fits; unit tests must pass under `go test -short ./...` without external services. Integration tests that need Postgres are gated behind the non-short suite.
- **No private deps:** this repo builds only against public modules + stdlib. If you reach for a private/internal library, stop and find a public equivalent.
