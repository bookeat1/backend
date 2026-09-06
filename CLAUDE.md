# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`backend-core` is the core backend service of **BookEat**. Go, Clean/Hexagonal architecture.

This is a **public** project. Do **not** add any private/internal dependencies (no private module registries, no company-internal libraries). Everything must be buildable from public modules and the Go standard library.

**Full architecture (layers, why they're shaped this way, data flow, decisions) lives in
`docs/ARCHITECTURE.md`, product requirements in `docs/PRD.md`** — read the relevant one before
any non-trivial change, especially anything touching a new layer, an external integration, or a
business rule. This file only holds what must be followed on every edit, kept short on purpose.

**Only `tech-lead` (ARCHITECTURE.md) and `product-manager` (PRD.md) edit those two files.**
Any other role that makes an architecturally or product-significant change notes what needs
updating in its PR/report instead of editing the doc directly — `project-manager` checks at
acceptance whether that update actually landed.

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

go run ./cmd/etl/main.go        # one-time Supabase dump → users ETL (see cmd/etl/README.md)
TEST_DATABASE_URL=... go test ./...   # integration tests (need a migrated Postgres)

go vet ./... && gofmt -w .
```

Config is **fully environment-variable based** — there is no config file. All entry points load it via **`bootstrap.NewConfig()`** (`internal/bootstrap/config.go`), which reads env vars with sane defaults and auto-loads a local `.env` when present (real env vars win over `.env`). Copy `.env.example` → `.env` for local development; never commit `.env`. Add new settings as typed fields on `Config` plus a `getEnv*` call in `NewConfig`.

## Hard rules while editing (details and rationale: ARCHITECTURE.md)

- Dependencies point **inward**: `transport → usecase → domain ← infrastructure`. `domain`
  imports nothing from outer layers and no frameworks. Wiring is assembled in
  **`internal/bootstrap/deps.go`** (`NewDeps`) — read it first to see how anything connects.
- `internal/domain/`: one file per entity, struct + repo interface + constants only, **no
  business logic, no framework imports**. Enumerated values are a named Go string type stored
  as `VARCHAR` — **never `CREATE TYPE ... AS ENUM`**. Sentinel errors live in `errors.go`.
- `internal/usecase/<pkg>/`: exported `Facade` interface + unexported `facade`, deps as
  **positional args** to `NewFacade(...)` (no `Deps` bundle, no exported `Service`). Complex
  operations get their own file + focused `...UseCase` interface next to the facade. A usecase
  **never imports another domain's concrete repository** — declare a local port in `ports.go`.
- `internal/transport/rest/`: `handler.go` + `request.go` (DTO, `Validate()`, `ToDomain()`) +
  `response.go` (`fromDomain()`). **All** responses go through `response.Envelope`, **all**
  errors through `response.HandleError` — always `return` right after writing an error.
  Error `code` is what clients branch on, never the message; a new narrower code
  (`domain.WithCode(...)`) is additive, changing/removing one is a breaking API change.
- `internal/infrastructure/`: implements domain interfaces, depends only on `domain`.
  `postgres/<entity>/repository.go` maps `23505` → `domain.ErrAlreadyExists`.
- Transactions: pull the active querier via `sqltx.From(ctx, r.pool)` so multi-repo work in a
  usecase shares one tx; nested `WithinTx` reuse the existing one.
- No private deps — this repo builds only against public modules + stdlib.

## Conventions

- **Errors:** return domain sentinel errors from usecases/repositories; never leak SQL or transport errors upward. Wrap with `fmt.Errorf("...: %w", err)` to preserve the sentinel for `errors.Is`.
- **Migrations:** SQL in `migrations/`, goose format (`-- +goose Up` / `-- +goose Down`), embedded via `migrations/embed.go`. `VARCHAR` for enumerated fields, validated in app code — no DB enums.
- **Naming:** package names are short, lower-case, no underscores. Interfaces are declared where they are **consumed** (the usecase/transport layer), not where implemented.
- **Formatting:** run `gofmt -w .` and `go vet ./...` before finishing any change.
- **Test doubles:** use **hand-written fakes**, not a mock-generation framework — this keeps the dependency set minimal. Put fakes in the consuming package's `*_test.go` (e.g. `usecase/auth/fakes_test.go`), implementing the small port/repository interfaces directly. Integration tests that need Postgres use `infrastructure/postgres/testdb` and are gated behind `TEST_DATABASE_URL` (skipped by `-short`).
- **Tests:** table-driven where it fits; unit tests must pass under `go test -short ./...` without external services. Integration tests that need Postgres are gated behind the non-short suite.
- **No private deps:** this repo builds only against public modules + stdlib. If you reach for a private/internal library, stop and find a public equivalent.
