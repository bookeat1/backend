# Users + Auth Extraction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `backend-core` its own identity provider — a clean `users` domain plus email/password and phone-OTP authentication issuing RS256 JWTs — seeded from a one-time Supabase dump, with the `book-eat-app` frontend untouched.

**Architecture:** Clean/Hexagonal per `backend-core/CLAUDE.md` (`transport → usecase → domain ← infrastructure`). Gin HTTP layer, `database/sql` over the pgx stdlib driver, goose migrations, RSA-signed JWTs, opaque rotating refresh tokens. Auth methods: email/password (bcrypt, compatible with imported Supabase hashes) and phone-OTP (delivery stubbed). One-time ETL loads a Supabase dump into a `raw_supabase` staging schema and transforms it into the clean tables.

**Tech Stack:** Go 1.25, Gin, pgx/v5 (stdlib driver), pressly/goose/v3, golang-jwt/jwt/v5, google/uuid, golang.org/x/crypto/bcrypt, go-playground/validator/v10.

**Reference spec:** `docs/superpowers/specs/2026-07-09-users-auth-extraction-design.md`

## Global Constraints

- **Public modules + stdlib only** — no private/internal dependencies (CLAUDE.md).
- **Layering:** dependencies point inward; `domain` imports no framework and no outer layer. Interfaces are declared where **consumed** (usecase/transport).
- **No DB enums** — enumerated fields are `VARCHAR`, validated in app code. Roles: `user`, `restaurant`, `admin`.
- **Preserve the Supabase user UUID** as `users.id` across the ETL.
- **Phone format:** E.164, default country code `+7` (KZ/RU), matching the frontend `normalizePhone`.
- **OTP:** 6 digits, TTL 300s, rate limit 1/min and 5/hour per phone; sha256-hashed at rest; delivery stubbed (log; dev-only response echo).
- **JWT:** RS256, access TTL 15m, claims `sub` (uuid string), `role`, `iat`, `exp`; public key served at `/.well-known/jwks.json`.
- **Every HTTP response** uses `response.Envelope`; **every error** goes through `response.HandleError`; always `return` immediately after writing an error.
- **Config** is env-only via `bootstrap.NewConfig()`; add typed fields + a `getEnv*` call; never commit secrets. `.env.example` documents every new var with a placeholder.
- **Formatting:** run `gofmt -w . && go vet ./...` before every commit.
- **Tests:** unit tests pass under `go test -short ./...` with no external services; integration tests needing Postgres are skipped when `-short` is set.
- Work happens on branch `feat/users-auth-extraction`.

---

## File Structure

**Foundation (Phase 0)**
- `go.mod` / `go.sum` — new dependencies.
- `internal/bootstrap/config.go` — add `AuthConfig` + `getEnvBool`.
- `internal/bootstrap/db.go` (create) — `NewDB(PostgresConfig) (*sql.DB, error)` pool.
- `.env.example` — new `AUTH_*` and confirm `DB_*` vars.
- `cmd/migrate/migrate.go` — real goose runner.
- `internal/infrastructure/sqltx/manager.go` (create) — `Manager` + `From(ctx)`.
- `migrations/0002_users_auth.sql` (create) — the four tables.

**Domain (Phase 1)**
- `internal/domain/user.go`, `user_credential.go`, `otp.go`, `refresh_token.go` (create).

**Pure helpers**
- `internal/auth/phone/phone.go` (+ test) — `Normalize`.
- `internal/auth/password/password.go` (+ test) — `Hash`, `Verify`.
- `internal/auth/otpcode/otpcode.go` (+ test) — `Generate`, `Hash`.

**Infrastructure**
- `internal/infrastructure/token/rsa.go` (+ test) — RSA JWT issuer/verifier + JWKS.
- `internal/infrastructure/postgres/user/repository.go` (+ test).
- `internal/infrastructure/postgres/usercredential/repository.go` (+ test).
- `internal/infrastructure/postgres/otp/repository.go` (+ test).
- `internal/infrastructure/postgres/refreshtoken/repository.go` (+ test).
- `internal/infrastructure/otpsender/stub.go`.
- `internal/infrastructure/postgres/testdb/testdb.go` — integration test helper.

**Usecase**
- `internal/usecase/auth/ports.go` — `TokenIssuer`, `OTPSender`, `TokenPair`.
- `internal/usecase/auth/service.go` (+ fakes + test) — signup/login/refresh/logout.
- `internal/usecase/auth/otp.go` (+ test) — request/verify OTP.
- `internal/usecase/users/facade.go` (+ test) — me / update.

**Transport**
- `internal/transport/rest/middleware/auth.go` — `Auth`, `GetAuthUser`, `AuthUser`.
- `internal/transport/rest/auth/{handler,request,response}.go`.
- `internal/transport/rest/users/{handler,request,response}.go`.

**Wiring & entry points**
- `internal/bootstrap/deps.go` (create) — `NewDeps`.
- `internal/bootstrap/app.go` (create) — Gin router, route groups, `Run`.
- `cmd/http/main.go` — call bootstrap app.
- `cmd/etl/main.go` (create) — dump → clean ETL.
- `docs/http/auth.http` (create) — manual request collection.
- `Makefile`, root `CLAUDE.md` — commands/docs.

---

## Task 1: Dependencies, auth config, DB connection pool

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `internal/bootstrap/config.go`
- Create: `internal/bootstrap/db.go`
- Modify: `.env.example`

**Interfaces:**
- Produces: `bootstrap.AuthConfig` struct (fields below); `Config.Auth AuthConfig`; `bootstrap.NewDB(cfg PostgresConfig) (*sql.DB, error)`.

- [ ] **Step 1: Add dependencies**

Run:
```bash
go get github.com/gin-gonic/gin@latest \
  github.com/jackc/pgx/v5@latest \
  github.com/pressly/goose/v3@latest \
  github.com/golang-jwt/jwt/v5@latest \
  github.com/google/uuid@latest \
  github.com/go-playground/validator/v10@latest \
  golang.org/x/crypto@latest
go mod tidy
```
Expected: `go.mod` lists the modules; `go.sum` updated.

- [ ] **Step 2: Add `AuthConfig` + `getEnvBool` to config.go**

In `internal/bootstrap/config.go`, add `Auth AuthConfig` to `Config`, define the struct, populate it in `NewConfig`, and add the bool helper:

```go
// add to Config struct:
//   Auth AuthConfig

type AuthConfig struct {
	JWTPrivateKeyPEM    string        // RSA private key (PEM). env: AUTH_JWT_PRIVATE_KEY
	JWTKeyID            string        // kid advertised in JWKS. env: AUTH_JWT_KID
	AccessTokenTTL      time.Duration // env: AUTH_ACCESS_TOKEN_TTL
	RefreshTokenTTL     time.Duration // env: AUTH_REFRESH_TOKEN_TTL
	OTPCodeTTL          time.Duration // env: AUTH_OTP_TTL
	OTPRateLimitPerMin  int           // env: AUTH_OTP_RATE_PER_MIN
	OTPRateLimitPerHour int           // env: AUTH_OTP_RATE_PER_HOUR
	OTPDevExpose        bool          // env: AUTH_OTP_DEV_EXPOSE — echo code in response (dev only)
}
```

In `NewConfig`, after the `DB` block, add:
```go
		Auth: AuthConfig{
			JWTPrivateKeyPEM:    getEnv("AUTH_JWT_PRIVATE_KEY", ""),
			JWTKeyID:            getEnv("AUTH_JWT_KID", "bookeat-dev"),
			AccessTokenTTL:      getEnvDuration("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL:     getEnvDuration("AUTH_REFRESH_TOKEN_TTL", 720*time.Hour),
			OTPCodeTTL:          getEnvDuration("AUTH_OTP_TTL", 5*time.Minute),
			OTPRateLimitPerMin:  getEnvInt("AUTH_OTP_RATE_PER_MIN", 1),
			OTPRateLimitPerHour: getEnvInt("AUTH_OTP_RATE_PER_HOUR", 5),
			OTPDevExpose:        getEnvBool("AUTH_OTP_DEV_EXPOSE", false),
		},
```

Add the helper next to the other `getEnv*` functions:
```go
// getEnvBool returns the boolean value of the environment variable named by
// key, or def when unset or unparseable. Accepts 1/t/true/0/f/false.
func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}
```

- [ ] **Step 3: Create the DB pool constructor**

Create `internal/bootstrap/db.go`:
```go
package bootstrap

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
)

// NewDB opens a pgx-backed *sql.DB pool configured from cfg and verifies
// connectivity with a Ping. The caller owns Close.
func NewDB(cfg PostgresConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}
```

- [ ] **Step 4: Document new env vars**

Append to `.env.example`:
```bash

# Auth
# RSA private key in PEM (generate: openssl genrsa 2048). One line with \n or a
# quoted multiline value. NEVER commit a real key.
AUTH_JWT_PRIVATE_KEY=
AUTH_JWT_KID=bookeat-dev
AUTH_ACCESS_TOKEN_TTL=15m
AUTH_REFRESH_TOKEN_TTL=720h
AUTH_OTP_TTL=5m
AUTH_OTP_RATE_PER_MIN=1
AUTH_OTP_RATE_PER_HOUR=5
AUTH_OTP_DEV_EXPOSE=false
```

- [ ] **Step 5: Verify build**

Run: `go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
gofmt -w .
git add go.mod go.sum internal/bootstrap/config.go internal/bootstrap/db.go .env.example
git commit -m "feat(bootstrap): add auth config, deps, and DB pool constructor"
```

---

## Task 2: Wire the goose migration runner

**Files:**
- Modify: `cmd/migrate/migrate.go`

**Interfaces:**
- Consumes: `bootstrap.NewConfig`, `bootstrap.NewDB`, `migrations.FS`.
- Produces: CLI `go run ./cmd/migrate/migrate.go [up|down|status]`.

- [ ] **Step 1: Implement the runner**

Replace the body of `cmd/migrate/migrate.go`:
```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/pressly/goose/v3"

	"backend-core/internal/bootstrap"
	"backend-core/migrations"
)

// Usage: go run ./cmd/migrate/migrate.go [up|down|status]
func main() {
	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	db, err := bootstrap.NewDB(cfg.DB.Postgres)
	if err != nil {
		slog.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		slog.Error("set dialect", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if err := goose.RunContext(context.Background(), cmd, db, "."); err != nil {
		slog.Error("migrate", slog.String("cmd", cmd), slog.String("error", err.Error()))
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Verify build + status against a running Postgres**

Run: `go build ./cmd/migrate/`
Expected: builds. (Full `status` run is exercised in Task 4 after a real migration exists.)

- [ ] **Step 3: Commit**

```bash
gofmt -w .
git add cmd/migrate/migrate.go
git commit -m "feat(migrate): wire goose runner over embedded migrations"
```

---

## Task 3: Transaction manager (`sqltx`)

**Files:**
- Create: `internal/infrastructure/sqltx/manager.go`

**Interfaces:**
- Consumes: `domain.TxManager` (`WithinTx(ctx, fn) error`).
- Produces: `sqltx.NewManager(*sql.DB) *Manager` (implements `domain.TxManager`); `sqltx.From(ctx) DBTX` returning the active `*sql.Tx` if present, else the pool. `DBTX` is the query interface repos depend on.

- [ ] **Step 1: Implement the manager**

Create `internal/infrastructure/sqltx/manager.go`:
```go
// Package sqltx provides the database transaction manager. WithinTx injects the
// active *sql.Tx into the context; repositories read it back via From so that
// multiple repos inside one usecase share a single transaction. Nested WithinTx
// calls reuse the existing tx (no double-begin).
package sqltx

import (
	"context"
	"database/sql"
)

// DBTX is the subset of *sql.DB / *sql.Tx that repositories use.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type ctxKey struct{}

// Manager implements domain.TxManager.
type Manager struct{ db *sql.DB }

func NewManager(db *sql.DB) *Manager { return &Manager{db: db} }

// WithinTx runs fn inside one transaction. If a tx is already active on ctx it
// reuses it (fn joins the outer transaction). Commits on nil, rolls back on error.
func (m *Manager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(ctxKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// From returns the active *sql.Tx from ctx, or the given pool when none is set.
func From(ctx context.Context, pool DBTX) DBTX {
	if tx, ok := ctx.Value(ctxKey{}).(*sql.Tx); ok {
		return tx
	}
	return pool
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
gofmt -w .
git add internal/infrastructure/sqltx/manager.go
git commit -m "feat(sqltx): add context-based transaction manager"
```

---

## Task 4: `users`/`auth` schema migration

**Files:**
- Create: `migrations/0002_users_auth.sql`

**Interfaces:**
- Produces: tables `users`, `user_credentials`, `otp_codes`, `refresh_tokens` in the app database.

- [ ] **Step 1: Write the migration**

Create `migrations/0002_users_auth.sql`:
```sql
-- +goose Up
CREATE TABLE users (
    id                  uuid PRIMARY KEY,
    email               varchar UNIQUE,
    phone               varchar UNIQUE,
    full_name           varchar NOT NULL DEFAULT '',
    role                varchar NOT NULL DEFAULT 'user',
    avatar_url          varchar,
    preferred_language  varchar NOT NULL DEFAULT 'ru',
    city                varchar,
    email_verified_at   timestamptz,
    phone_verified_at   timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_credentials (
    user_id       uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash varchar NOT NULL
);

CREATE TABLE otp_codes (
    id         uuid PRIMARY KEY,
    phone      varchar NOT NULL,
    code_hash  varchar NOT NULL,
    channel    varchar,
    attempts   int NOT NULL DEFAULT 0,
    used_at    timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_otp_codes_phone_created ON otp_codes (phone, created_at DESC);
CREATE INDEX idx_otp_codes_expires ON otp_codes (expires_at);

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash varchar NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    user_agent varchar,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE refresh_tokens;
DROP TABLE otp_codes;
DROP TABLE user_credentials;
DROP TABLE users;
```

- [ ] **Step 2: Apply against a local Postgres**

Ensure a local Postgres matching `.env` defaults is running (`bookeat` db). Then run:
```bash
go run ./cmd/migrate/migrate.go up
go run ./cmd/migrate/migrate.go status
```
Expected: `0002_users_auth.sql` shows as applied; four tables exist.

- [ ] **Step 3: Commit**

```bash
git add migrations/0002_users_auth.sql
git commit -m "feat(migrations): add users, credentials, otp, refresh_token tables"
```

---

## Task 5: Domain entities & repository interfaces

**Files:**
- Create: `internal/domain/user.go`, `internal/domain/user_credential.go`, `internal/domain/otp.go`, `internal/domain/refresh_token.go`

**Interfaces:**
- Produces: the four entity structs + repository interfaces below. Later tasks depend on these exact signatures.

- [ ] **Step 1: `user.go`**

```go
package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Role is a user's authorization level, stored as VARCHAR.
type Role string

const (
	RoleUser       Role = "user"
	RoleRestaurant Role = "restaurant"
	RoleAdmin      Role = "admin"
)

// User is a person who can authenticate. Email and Phone are optional but at
// least one is always present. ID equals the original Supabase auth.users id.
type User struct {
	ID                uuid.UUID
	Email             *string
	Phone             *string
	FullName          string
	Role              Role
	AvatarURL         *string
	PreferredLanguage string
	City              *string
	EmailVerifiedAt   *time.Time
	PhoneVerifiedAt   *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// UserRepository persists users. Get* return ErrNotFound when absent.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByPhone(ctx context.Context, phone string) (*User, error)
	Update(ctx context.Context, u *User) error
}
```

- [ ] **Step 2: `user_credential.go`**

```go
package domain

import (
	"context"

	"github.com/google/uuid"
)

// UserCredential holds a user's bcrypt password hash. Absent for OTP-only users.
type UserCredential struct {
	UserID       uuid.UUID
	PasswordHash string
}

// UserCredentialRepository persists password hashes. GetByUserID returns
// ErrNotFound when the user has no password credential.
type UserCredentialRepository interface {
	Upsert(ctx context.Context, c *UserCredential) error
	GetByUserID(ctx context.Context, userID uuid.UUID) (*UserCredential, error)
}
```

- [ ] **Step 3: `otp.go`**

```go
package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// OTPCode is a single issued phone one-time code. CodeHash is sha256(code).
type OTPCode struct {
	ID        uuid.UUID
	Phone     string
	CodeHash  string
	Channel   string
	Attempts  int
	UsedAt    *time.Time
	ExpiresAt time.Time
	CreatedAt time.Time
}

// OTPRepository persists OTP codes.
type OTPRepository interface {
	Create(ctx context.Context, c *OTPCode) error
	// LatestActiveByPhone returns the newest unused, unexpired code for phone,
	// or ErrNotFound.
	LatestActiveByPhone(ctx context.Context, phone string) (*OTPCode, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
	IncrementAttempts(ctx context.Context, id uuid.UUID) error
	// CountSince counts codes created for phone at or after ts (for rate limits).
	CountSince(ctx context.Context, phone string, ts time.Time) (int, error)
}
```

- [ ] **Step 4: `refresh_token.go`**

```go
package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RefreshToken is a hashed, rotating refresh credential.
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	UserAgent *string
	CreatedAt time.Time
}

// RefreshTokenRepository persists refresh tokens. GetByHash returns ErrNotFound
// when no row matches.
type RefreshTokenRepository interface {
	Create(ctx context.Context, t *RefreshToken) error
	GetByHash(ctx context.Context, tokenHash string) (*RefreshToken, error)
	Revoke(ctx context.Context, id uuid.UUID) error
}
```

- [ ] **Step 5: Verify build**

Run: `go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
gofmt -w .
git add internal/domain/user.go internal/domain/user_credential.go internal/domain/otp.go internal/domain/refresh_token.go
git commit -m "feat(domain): add user, credential, otp, refresh_token entities"
```

---

## Task 6: Pure helpers — phone, password, otpcode

**Files:**
- Create: `internal/auth/phone/phone.go`, `internal/auth/phone/phone_test.go`
- Create: `internal/auth/password/password.go`, `internal/auth/password/password_test.go`
- Create: `internal/auth/otpcode/otpcode.go`, `internal/auth/otpcode/otpcode_test.go`

**Interfaces:**
- Produces: `phone.Normalize(raw string) string`; `password.Hash(plain string) (string, error)`, `password.Verify(hash, plain string) bool`; `otpcode.Generate() (string, error)`, `otpcode.Hash(code string) string`.

- [ ] **Step 1: Write the phone test**

Create `internal/auth/phone/phone_test.go`:
```go
package phone

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"8 707 123 4567":  "+77071234567",
		"+7 707 123 4567": "+77071234567",
		"77071234567":     "+77071234567",
		"7071234567":      "+77071234567",
		"":                "",
		"+1 202 555 0100": "+12025550100",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test — expect fail (no package)**

Run: `go test ./internal/auth/phone/`
Expected: FAIL / build error `undefined: Normalize`.

- [ ] **Step 3: Implement phone.Normalize**

Create `internal/auth/phone/phone.go` (mirrors the frontend `normalizePhone`):
```go
// Package phone normalizes user-typed phone numbers to E.164, matching the
// frontend's normalizePhone (default country code +7 for the KZ/RU market).
package phone

import "strings"

// Normalize returns raw as E.164 ("+7..."), or "" when raw has no digits.
func Normalize(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if d == "" {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "+") {
		return "+" + d
	}
	switch {
	case len(d) == 11 && d[0] == '8':
		return "+7" + d[1:]
	case len(d) == 11 && d[0] == '7':
		return "+" + d
	case len(d) == 10:
		return "+7" + d
	default:
		return "+" + d
	}
}
```

- [ ] **Step 4: Run phone test — expect pass**

Run: `go test ./internal/auth/phone/`
Expected: PASS.

- [ ] **Step 5: Write the password test**

Create `internal/auth/password/password_test.go`:
```go
package password

import "testing"

func TestHashAndVerify(t *testing.T) {
	h, err := Hash("s3cret-pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !Verify(h, "s3cret-pw") {
		t.Error("Verify should accept the correct password")
	}
	if Verify(h, "wrong") {
		t.Error("Verify should reject a wrong password")
	}
}

// A bcrypt hash produced by Supabase (GoTrue) must verify — proves password
// migration works without a reset. Hash of "password" at cost 10.
func TestVerifySupabaseStyleHash(t *testing.T) {
	const supa = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	if !Verify(supa, "password") {
		t.Error("expected imported $2a$ bcrypt hash to verify")
	}
}
```

- [ ] **Step 6: Run — expect fail**

Run: `go test ./internal/auth/password/`
Expected: FAIL / build error `undefined: Hash`.

- [ ] **Step 7: Implement password**

Create `internal/auth/password/password.go`:
```go
// Package password wraps bcrypt. It verifies both $2a$ (Supabase/GoTrue) and
// $2b$ hashes, so passwords imported from Supabase keep working.
package password

import "golang.org/x/crypto/bcrypt"

// Hash returns a bcrypt hash of plain at the default cost.
func Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// Verify reports whether plain matches the bcrypt hash.
func Verify(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
```

- [ ] **Step 8: Run — expect pass**

Run: `go test ./internal/auth/password/`
Expected: PASS (both tests).

- [ ] **Step 9: Write the otpcode test**

Create `internal/auth/otpcode/otpcode_test.go`:
```go
package otpcode

import (
	"regexp"
	"testing"
)

func TestGenerateIsSixDigits(t *testing.T) {
	re := regexp.MustCompile(`^\d{6}$`)
	for i := 0; i < 50; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !re.MatchString(c) {
			t.Fatalf("Generate() = %q, want 6 digits", c)
		}
	}
}

func TestHashIsStableAndDistinct(t *testing.T) {
	if Hash("123456") != Hash("123456") {
		t.Error("Hash must be deterministic")
	}
	if Hash("123456") == Hash("654321") {
		t.Error("different codes must hash differently")
	}
}
```

- [ ] **Step 10: Run — expect fail**

Run: `go test ./internal/auth/otpcode/`
Expected: FAIL / build error.

- [ ] **Step 11: Implement otpcode**

Create `internal/auth/otpcode/otpcode.go`:
```go
// Package otpcode generates 6-digit numeric OTPs and hashes them with sha256
// (codes are never stored in the clear), mirroring the Supabase edge function.
package otpcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const length = 6

// Generate returns a cryptographically random 6-digit code (zero-padded).
func Generate() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(b[:]) % 1_000_000
	return fmt.Sprintf("%0*d", length, n), nil
}

// Hash returns the hex sha256 of code.
func Hash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 12: Run — expect pass**

Run: `go test ./internal/auth/...`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
gofmt -w .
git add internal/auth/
git commit -m "feat(auth): phone normalize, bcrypt password, otp code helpers"
```

---

## Task 7: RSA JWT issuer/verifier + JWKS

**Files:**
- Create: `internal/infrastructure/token/rsa.go`, `internal/infrastructure/token/rsa_test.go`

**Interfaces:**
- Consumes: none (leaf infra).
- Produces: `token.NewRSAIssuer(privatePEM, kid string, ttl time.Duration) (*RSAIssuer, error)`; methods `IssueAccess(userID uuid.UUID, role string) (string, time.Time, error)`, `ParseAccess(tok string) (uuid.UUID, string, error)`, `JWKS() map[string]any`. This type satisfies the `auth.TokenIssuer` port defined in Task 11.
- Test helper: `token.GenerateTestKeyPEM(t) string` (exported for reuse by later tests).

- [ ] **Step 1: Write the test**

Create `internal/infrastructure/token/rsa_test.go`:
```go
package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/google/uuid"
)

// GenerateTestKeyPEM returns a fresh RSA private key in PKCS#8 PEM for tests.
func GenerateTestKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestIssueAndParseRoundTrip(t *testing.T) {
	iss, err := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", 15*time.Minute)
	if err != nil {
		t.Fatalf("NewRSAIssuer: %v", err)
	}
	id := uuid.New()
	tok, exp, err := iss.IssueAccess(id, "user")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Error("expiry should be in the future")
	}
	gotID, gotRole, err := iss.ParseAccess(tok)
	if err != nil {
		t.Fatalf("ParseAccess: %v", err)
	}
	if gotID != id || gotRole != "user" {
		t.Errorf("round trip = %v/%q, want %v/user", gotID, gotRole, id)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	iss, _ := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", time.Minute)
	if _, _, err := iss.ParseAccess("not.a.jwt"); err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestJWKSExposesKey(t *testing.T) {
	iss, _ := NewRSAIssuer(GenerateTestKeyPEM(t), "kid-1", time.Minute)
	jwks := iss.JWKS()
	keys, ok := jwks["keys"].([]map[string]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("JWKS keys malformed: %#v", jwks)
	}
	if keys[0]["kid"] != "kid-1" || keys[0]["kty"] != "RSA" {
		t.Errorf("unexpected jwk: %#v", keys[0])
	}
}
```

- [ ] **Step 2: Run — expect fail**

Run: `go test ./internal/infrastructure/token/`
Expected: FAIL / build error `undefined: NewRSAIssuer`.

- [ ] **Step 3: Implement the issuer**

Create `internal/infrastructure/token/rsa.go`:
```go
// Package token issues and verifies RS256 access JWTs and exposes the public
// key as a JWKS document.
package token

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// RSAIssuer signs access tokens with an RSA private key.
type RSAIssuer struct {
	key *rsa.PrivateKey
	kid string
	ttl time.Duration
}

// NewRSAIssuer parses a PKCS#8 (or PKCS#1) RSA private key PEM.
func NewRSAIssuer(privatePEM, kid string, ttl time.Duration) (*RSAIssuer, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("token: invalid private key PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("token: PKCS8 key is not RSA")
		}
		key = rk
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, fmt.Errorf("token: parse private key: %w", err)
	}
	return &RSAIssuer{key: key, kid: kid, ttl: ttl}, nil
}

// IssueAccess returns a signed token, its expiry, and any error.
func (i *RSAIssuer) IssueAccess(userID uuid.UUID, role string) (string, time.Time, error) {
	exp := time.Now().Add(i.ttl)
	claims := jwt.MapClaims{
		"sub":  userID.String(),
		"role": role,
		"iat":  time.Now().Unix(),
		"exp":  exp.Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = i.kid
	signed, err := t.SignedString(i.key)
	return signed, exp, err
}

// ParseAccess verifies signature + expiry and returns the subject and role.
func (i *RSAIssuer) ParseAccess(tok string) (uuid.UUID, string, error) {
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return &i.key.PublicKey, nil
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return uuid.Nil, "", errors.New("token: invalid claims")
	}
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, "", errors.New("token: invalid sub")
	}
	role, _ := claims["role"].(string)
	return id, role, nil
}

// JWKS returns the public key as a JWKS document for /.well-known/jwks.json.
func (i *RSAIssuer) JWKS() map[string]any {
	pub := i.key.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": i.kid, "n": n, "e": e,
		}},
	}
}
```

- [ ] **Step 4: Run — expect pass**

Run: `go test ./internal/infrastructure/token/`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add internal/infrastructure/token/
git commit -m "feat(token): RS256 access token issuer/verifier + JWKS"
```

---

## Task 8: Postgres repos — user & user_credential

**Files:**
- Create: `internal/infrastructure/postgres/testdb/testdb.go`
- Create: `internal/infrastructure/postgres/user/repository.go`, `.../user/repository_test.go`
- Create: `internal/infrastructure/postgres/usercredential/repository.go`, `.../usercredential/repository_test.go`

**Interfaces:**
- Consumes: `sqltx.DBTX`, `sqltx.From`, `domain.UserRepository`, `domain.UserCredentialRepository`, `domain.ErrNotFound`.
- Produces: `user.New(pool sqltx.DBTX) *Repository`; `usercredential.New(pool sqltx.DBTX) *Repository`. `testdb.Connect(t) *sql.DB` (skips when `-short` or no `TEST_DATABASE_URL`).

- [ ] **Step 1: Integration test helper**

Create `internal/infrastructure/postgres/testdb/testdb.go`:
```go
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
```

- [ ] **Step 2: Write the user repo test**

Create `internal/infrastructure/postgres/user/repository_test.go`:
```go
package user

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func strp(s string) *string { return &s }

func TestCreateGetUpdate(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	u := &domain.User{ID: uuid.New(), Email: strp("a@b.com"), FullName: "Alice", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FullName != "Alice" || got.Email == nil || *got.Email != "a@b.com" {
		t.Errorf("unexpected user: %+v", got)
	}

	byEmail, err := repo.GetByEmail(ctx, "a@b.com")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("GetByEmail: %v / %+v", err, byEmail)
	}

	u.FullName = "Alice B"
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = repo.GetByID(ctx, u.ID)
	if got.FullName != "Alice B" {
		t.Errorf("update not persisted: %q", got.FullName)
	}
}

func TestGetByIDNotFound(t *testing.T) {
	db := testdb.Connect(t)
	repo := New(db)
	if _, err := repo.GetByID(context.Background(), uuid.New()); err == nil {
		t.Error("expected ErrNotFound")
	}
}
```

- [ ] **Step 3: Run — expect fail**

Run: `go test ./internal/infrastructure/postgres/user/`
Expected: FAIL / build error `undefined: New` (or skip if no test DB — set `TEST_DATABASE_URL` to run).

- [ ] **Step 4: Implement the user repo**

Create `internal/infrastructure/postgres/user/repository.go`:
```go
// Package user is the Postgres implementation of domain.UserRepository.
package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

const columns = `id, email, phone, full_name, role, avatar_url,
	preferred_language, city, email_verified_at, phone_verified_at,
	created_at, updated_at`

func (r *Repository) Create(ctx context.Context, u *domain.User) error {
	q := `INSERT INTO users (` + columns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.AvatarURL,
		u.PreferredLanguage, u.City, u.EmailVerifiedAt, u.PhoneVerifiedAt,
		u.CreatedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return r.getBy(ctx, "id = $1", id)
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.getBy(ctx, "email = $1", email)
}

func (r *Repository) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return r.getBy(ctx, "phone = $1", phone)
}

func (r *Repository) getBy(ctx context.Context, where string, arg any) (*domain.User, error) {
	q := `SELECT ` + columns + ` FROM users WHERE ` + where
	row := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, arg)
	u, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (r *Repository) Update(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = time.Now()
	q := `UPDATE users SET email=$2, phone=$3, full_name=$4, role=$5,
		avatar_url=$6, preferred_language=$7, city=$8, email_verified_at=$9,
		phone_verified_at=$10, updated_at=$11 WHERE id=$1`
	res, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.AvatarURL,
		u.PreferredLanguage, u.City, u.EmailVerifiedAt, u.PhoneVerifiedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*domain.User, error) {
	var u domain.User
	var role string
	if err := row.Scan(&u.ID, &u.Email, &u.Phone, &u.FullName, &role,
		&u.AvatarURL, &u.PreferredLanguage, &u.City, &u.EmailVerifiedAt,
		&u.PhoneVerifiedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.Role = domain.Role(role)
	return &u, nil
}
```

- [ ] **Step 5: Run user repo test — expect pass (with test DB)**

Run: `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./internal/infrastructure/postgres/user/`
Expected: PASS.

- [ ] **Step 6: Write the credential repo test**

Create `internal/infrastructure/postgres/usercredential/repository_test.go`:
```go
package usercredential

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
)

func TestUpsertAndGet(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	id := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: id, FullName: "X", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	repo := New(db)
	if err := repo.Upsert(ctx, &domain.UserCredential{UserID: id, PasswordHash: "h1"}); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	if err := repo.Upsert(ctx, &domain.UserCredential{UserID: id, PasswordHash: "h2"}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, err := repo.GetByUserID(ctx, id)
	if err != nil || got.PasswordHash != "h2" {
		t.Fatalf("GetByUserID = %+v, %v", got, err)
	}
	if _, err := repo.GetByUserID(ctx, uuid.New()); err == nil {
		t.Error("expected ErrNotFound for unknown user")
	}
}
```

- [ ] **Step 7: Run — expect fail**

Run: `go test ./internal/infrastructure/postgres/usercredential/`
Expected: FAIL / build error `undefined: New`.

- [ ] **Step 8: Implement the credential repo**

Create `internal/infrastructure/postgres/usercredential/repository.go`:
```go
// Package usercredential is the Postgres implementation of
// domain.UserCredentialRepository.
package usercredential

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

func (r *Repository) Upsert(ctx context.Context, c *domain.UserCredential) error {
	q := `INSERT INTO user_credentials (user_id, password_hash)
		VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash`
	if _, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q, c.UserID, c.PasswordHash); err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}
	return nil
}

func (r *Repository) GetByUserID(ctx context.Context, userID uuid.UUID) (*domain.UserCredential, error) {
	q := `SELECT user_id, password_hash FROM user_credentials WHERE user_id = $1`
	var c domain.UserCredential
	err := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, userID).Scan(&c.UserID, &c.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	return &c, nil
}
```

- [ ] **Step 9: Run — expect pass (with test DB)**

Run: `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./internal/infrastructure/postgres/...`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
gofmt -w .
git add internal/infrastructure/postgres/
git commit -m "feat(postgres): user and user_credential repositories + test harness"
```

---

## Task 9: Postgres repos — otp & refresh_token

**Files:**
- Create: `internal/infrastructure/postgres/otp/repository.go`, `.../otp/repository_test.go`
- Create: `internal/infrastructure/postgres/refreshtoken/repository.go`, `.../refreshtoken/repository_test.go`

**Interfaces:**
- Consumes: `domain.OTPRepository`, `domain.RefreshTokenRepository`, `sqltx.From`, `domain.ErrNotFound`.
- Produces: `otp.New(pool sqltx.DBTX) *Repository`; `refreshtoken.New(pool sqltx.DBTX) *Repository`.

- [ ] **Step 1: Write the otp repo test**

Create `internal/infrastructure/postgres/otp/repository_test.go`:
```go
package otp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestCreateLatestActiveAndUse(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "otp_codes")
	repo := New(db)
	ctx := context.Background()

	c := &domain.OTPCode{ID: uuid.New(), Phone: "+77070000000", CodeHash: "h", Channel: "stub", ExpiresAt: time.Now().Add(5 * time.Minute)}
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.LatestActiveByPhone(ctx, "+77070000000")
	if err != nil || got.ID != c.ID {
		t.Fatalf("LatestActiveByPhone = %+v, %v", got, err)
	}

	if err := repo.IncrementAttempts(ctx, c.ID); err != nil {
		t.Fatalf("IncrementAttempts: %v", err)
	}
	if err := repo.MarkUsed(ctx, c.ID); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if _, err := repo.LatestActiveByPhone(ctx, "+77070000000"); err == nil {
		t.Error("used code must not be active")
	}

	n, err := repo.CountSince(ctx, "+77070000000", time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CountSince = %d, %v", n, err)
	}
}
```

- [ ] **Step 2: Run — expect fail**

Run: `go test ./internal/infrastructure/postgres/otp/`
Expected: FAIL / build error `undefined: New`.

- [ ] **Step 3: Implement the otp repo**

Create `internal/infrastructure/postgres/otp/repository.go`:
```go
// Package otp is the Postgres implementation of domain.OTPRepository.
package otp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

func (r *Repository) Create(ctx context.Context, c *domain.OTPCode) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	q := `INSERT INTO otp_codes (id, phone, code_hash, channel, attempts, used_at, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		c.ID, c.Phone, c.CodeHash, c.Channel, c.Attempts, c.UsedAt, c.ExpiresAt, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("create otp: %w", err)
	}
	return nil
}

func (r *Repository) LatestActiveByPhone(ctx context.Context, phone string) (*domain.OTPCode, error) {
	q := `SELECT id, phone, code_hash, channel, attempts, used_at, expires_at, created_at
		FROM otp_codes
		WHERE phone = $1 AND used_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1`
	var c domain.OTPCode
	err := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, phone).Scan(
		&c.ID, &c.Phone, &c.CodeHash, &c.Channel, &c.Attempts, &c.UsedAt, &c.ExpiresAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("latest otp: %w", err)
	}
	return &c, nil
}

func (r *Repository) MarkUsed(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx,
		`UPDATE otp_codes SET used_at = now() WHERE id = $1`, id)
	return err
}

func (r *Repository) IncrementAttempts(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx,
		`UPDATE otp_codes SET attempts = attempts + 1 WHERE id = $1`, id)
	return err
}

func (r *Repository) CountSince(ctx context.Context, phone string, ts time.Time) (int, error) {
	var n int
	err := sqltx.From(ctx, r.pool).QueryRowContext(ctx,
		`SELECT count(*) FROM otp_codes WHERE phone = $1 AND created_at >= $2`, phone, ts).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count otp: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 4: Run — expect pass (with test DB)**

Run: `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./internal/infrastructure/postgres/otp/`
Expected: PASS.

- [ ] **Step 5: Write the refresh_token repo test**

Create `internal/infrastructure/postgres/refreshtoken/repository_test.go`:
```go
package refreshtoken

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
)

func TestCreateGetRevoke(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()

	uid := uuid.New()
	if err := userrepo.New(db).Create(ctx, &domain.User{ID: uid, FullName: "X", Role: domain.RoleUser, PreferredLanguage: "ru"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	repo := New(db)
	rt := &domain.RefreshToken{ID: uuid.New(), UserID: uid, TokenHash: "th", ExpiresAt: time.Now().Add(time.Hour)}
	if err := repo.Create(ctx, rt); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.GetByHash(ctx, "th")
	if err != nil || got.ID != rt.ID {
		t.Fatalf("GetByHash = %+v, %v", got, err)
	}
	if err := repo.Revoke(ctx, rt.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ = repo.GetByHash(ctx, "th")
	if got.RevokedAt == nil {
		t.Error("expected RevokedAt to be set after Revoke")
	}
	if _, err := repo.GetByHash(ctx, "missing"); err == nil {
		t.Error("expected ErrNotFound for unknown hash")
	}
}
```

- [ ] **Step 6: Run — expect fail**

Run: `go test ./internal/infrastructure/postgres/refreshtoken/`
Expected: FAIL / build error `undefined: New`.

- [ ] **Step 7: Implement the refresh_token repo**

Create `internal/infrastructure/postgres/refreshtoken/repository.go`:
```go
// Package refreshtoken is the Postgres implementation of
// domain.RefreshTokenRepository.
package refreshtoken

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

func (r *Repository) Create(ctx context.Context, t *domain.RefreshToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	q := `INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at, user_agent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		t.ID, t.UserID, t.TokenHash, t.ExpiresAt, t.RevokedAt, t.UserAgent, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *Repository) GetByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error) {
	q := `SELECT id, user_id, token_hash, expires_at, revoked_at, user_agent, created_at
		FROM refresh_tokens WHERE token_hash = $1`
	var t domain.RefreshToken
	err := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, tokenHash).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.RevokedAt, &t.UserAgent, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", err)
	}
	return &t, nil
}

func (r *Repository) Revoke(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, id)
	return err
}
```

- [ ] **Step 8: Run — expect pass (with test DB)**

Run: `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./internal/infrastructure/postgres/...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
gofmt -w .
git add internal/infrastructure/postgres/otp/ internal/infrastructure/postgres/refreshtoken/
git commit -m "feat(postgres): otp and refresh_token repositories"
```

---

## Task 10: OTP stub sender

**Files:**
- Create: `internal/infrastructure/otpsender/stub.go`

**Interfaces:**
- Produces: `otpsender.NewStub(log *slog.Logger) *Stub` with `Send(ctx, phone, code string) (channel string, err error)` returning channel `"stub"`. Satisfies the `auth.OTPSender` port (Task 11).

- [ ] **Step 1: Implement the stub sender**

Create `internal/infrastructure/otpsender/stub.go`:
```go
// Package otpsender delivers OTP codes. The stub logs the code instead of
// sending it — the real provider waterfall (Telegram / Gateway / WhatsApp / SMS)
// is a later phase.
package otpsender

import (
	"context"
	"log/slog"
)

type Stub struct{ log *slog.Logger }

func NewStub(log *slog.Logger) *Stub { return &Stub{log: log} }

// Send logs the code and reports the "stub" channel. Never errors.
func (s *Stub) Send(ctx context.Context, phone, code string) (string, error) {
	s.log.Info("otp stub send", slog.String("phone", phone), slog.String("code", code))
	return "stub", nil
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
gofmt -w .
git add internal/infrastructure/otpsender/
git commit -m "feat(otpsender): stub OTP delivery that logs the code"
```

---

## Task 11: Auth usecase — signup, login, refresh, logout

**Files:**
- Create: `internal/usecase/auth/ports.go`
- Create: `internal/usecase/auth/service.go`
- Create: `internal/usecase/auth/fakes_test.go`
- Create: `internal/usecase/auth/service_test.go`

**Interfaces:**
- Consumes: `domain.UserRepository`, `domain.UserCredentialRepository`, `domain.RefreshTokenRepository`, `domain.OTPRepository`, `domain.TxManager`, `password.*`, plus the ports below.
- Produces:
  - Ports: `TokenIssuer{ IssueAccess(uuid.UUID, string) (string, time.Time, error); ParseAccess(string) (uuid.UUID, string, error) }`, `OTPSender{ Send(context.Context, string, string) (string, error) }`.
  - `TokenPair{ AccessToken string; RefreshToken string; ExpiresAt time.Time }`.
  - `auth.Config{ RefreshTTL, OTPTTL time.Duration; OTPPerMin, OTPPerHour int; OTPDevExpose bool }`.
  - `auth.NewService(deps Deps) *Service` where `Deps` bundles repos + `TxManager` + `TokenIssuer` + `OTPSender` + `Config`.
  - Methods: `Signup(ctx, email, pw, fullName string) (*TokenPair, error)`, `Login(ctx, email, pw string) (*TokenPair, error)`, `Refresh(ctx, refresh string) (*TokenPair, error)`, `Logout(ctx, refresh string) error`. (OTP methods added in Task 12.)

- [ ] **Step 1: Define ports and shared types**

Create `internal/usecase/auth/ports.go`:
```go
// Package auth is the authentication application logic: password + phone-OTP
// login, JWT issuance, and refresh-token rotation.
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TokenIssuer issues and verifies access tokens. Implemented by
// infrastructure/token.RSAIssuer.
type TokenIssuer interface {
	IssueAccess(userID uuid.UUID, role string) (string, time.Time, error)
	ParseAccess(token string) (uuid.UUID, string, error)
}

// OTPSender delivers an OTP code and returns the channel used. Implemented by
// infrastructure/otpsender.Stub.
type OTPSender interface {
	Send(ctx context.Context, phone, code string) (string, error)
}

// TokenPair is the credential set returned to a client on successful auth.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Config holds auth timing and OTP policy.
type Config struct {
	RefreshTTL   time.Duration
	OTPTTL       time.Duration
	OTPPerMin    int
	OTPPerHour   int
	OTPDevExpose bool
}

// Deps bundles everything the Service needs. Wired in bootstrap.NewDeps.
type Deps struct {
	Users       domain.UserRepository
	Credentials domain.UserCredentialRepository
	Refresh     domain.RefreshTokenRepository
	OTP         domain.OTPRepository
	Tx          domain.TxManager
	Tokens      TokenIssuer
	OTPSender   OTPSender
	Config      Config
}

// Service implements the auth usecases.
type Service struct{ d Deps }

func NewService(d Deps) *Service { return &Service{d: d} }
```

- [ ] **Step 2: Write in-memory fakes for tests**

Create `internal/usecase/auth/fakes_test.go`:
```go
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeUsers is an in-memory domain.UserRepository.
type fakeUsers struct{ byID map[uuid.UUID]*domain.User }

func newFakeUsers() *fakeUsers { return &fakeUsers{byID: map[uuid.UUID]*domain.User{}} }

func (f *fakeUsers) Create(_ context.Context, u *domain.User) error {
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}
func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	for _, u := range f.byID {
		if u.Email != nil && *u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	for _, u := range f.byID {
		if u.Phone != nil && *u.Phone == phone {
			cp := *u
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) Update(_ context.Context, u *domain.User) error {
	if _, ok := f.byID[u.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *u
	f.byID[u.ID] = &cp
	return nil
}

type fakeCreds struct{ byUser map[uuid.UUID]string }

func newFakeCreds() *fakeCreds { return &fakeCreds{byUser: map[uuid.UUID]string{}} }
func (f *fakeCreds) Upsert(_ context.Context, c *domain.UserCredential) error {
	f.byUser[c.UserID] = c.PasswordHash
	return nil
}
func (f *fakeCreds) GetByUserID(_ context.Context, id uuid.UUID) (*domain.UserCredential, error) {
	if h, ok := f.byUser[id]; ok {
		return &domain.UserCredential{UserID: id, PasswordHash: h}, nil
	}
	return nil, domain.ErrNotFound
}

type fakeRefresh struct{ byHash map[string]*domain.RefreshToken }

func newFakeRefresh() *fakeRefresh { return &fakeRefresh{byHash: map[string]*domain.RefreshToken{}} }
func (f *fakeRefresh) Create(_ context.Context, t *domain.RefreshToken) error {
	cp := *t
	f.byHash[t.TokenHash] = &cp
	return nil
}
func (f *fakeRefresh) GetByHash(_ context.Context, h string) (*domain.RefreshToken, error) {
	if t, ok := f.byHash[h]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeRefresh) Revoke(_ context.Context, id uuid.UUID) error {
	for _, t := range f.byHash {
		if t.ID == id {
			now := time.Now()
			t.RevokedAt = &now
		}
	}
	return nil
}

// fakeOTP is defined here; exercised in Task 12.
type fakeOTP struct{ codes []*domain.OTPCode }

func newFakeOTP() *fakeOTP { return &fakeOTP{} }
func (f *fakeOTP) Create(_ context.Context, c *domain.OTPCode) error {
	cp := *c
	f.codes = append(f.codes, &cp)
	return nil
}
func (f *fakeOTP) LatestActiveByPhone(_ context.Context, phone string) (*domain.OTPCode, error) {
	for i := len(f.codes) - 1; i >= 0; i-- {
		c := f.codes[i]
		if c.Phone == phone && c.UsedAt == nil && c.ExpiresAt.After(time.Now()) {
			return c, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeOTP) MarkUsed(_ context.Context, id uuid.UUID) error {
	for _, c := range f.codes {
		if c.ID == id {
			now := time.Now()
			c.UsedAt = &now
		}
	}
	return nil
}
func (f *fakeOTP) IncrementAttempts(_ context.Context, id uuid.UUID) error {
	for _, c := range f.codes {
		if c.ID == id {
			c.Attempts++
		}
	}
	return nil
}
func (f *fakeOTP) CountSince(_ context.Context, phone string, ts time.Time) (int, error) {
	n := 0
	for _, c := range f.codes {
		if c.Phone == phone && !c.CreatedAt.Before(ts) {
			n++
		}
	}
	return n, nil
}

// noTx runs fn directly (no real transaction) — fine for unit tests.
type noTx struct{}

func (noTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// stubSender records nothing and returns channel "test".
type stubSender struct{ lastCode string }

func (s *stubSender) Send(_ context.Context, _ , code string) (string, error) {
	s.lastCode = code
	return "test", nil
}

// realIssuer builds a real RSAIssuer for tests via the token package helper.
```

- [ ] **Step 3: Write service tests (signup/login/refresh/logout)**

Create `internal/usecase/auth/service_test.go`:
```go
package auth

import (
	"context"
	"testing"
	"time"

	"backend-core/internal/infrastructure/token"
)

func newTestService(t *testing.T) (*Service, *stubSender) {
	t.Helper()
	iss, err := token.NewRSAIssuer(token.GenerateTestKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	sender := &stubSender{}
	return NewService(Deps{
		Users:       newFakeUsers(),
		Credentials: newFakeCreds(),
		Refresh:     newFakeRefresh(),
		OTP:         newFakeOTP(),
		Tx:          noTx{},
		Tokens:      iss,
		OTPSender:   sender,
		Config:      Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5},
	}), sender
}

func TestSignupThenLogin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Signup(ctx, "a@b.com", "pw12345", "Alice")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected non-empty token pair")
	}

	if _, err := svc.Signup(ctx, "a@b.com", "pw", "Dup"); err == nil {
		t.Error("expected ErrAlreadyExists on duplicate email")
	}

	if _, err := svc.Login(ctx, "a@b.com", "pw12345"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := svc.Login(ctx, "a@b.com", "wrong"); err == nil {
		t.Error("expected error on wrong password")
	}
	if _, err := svc.Login(ctx, "nobody@b.com", "pw"); err == nil {
		t.Error("expected error on unknown email")
	}
}

func TestRefreshRotatesAndRevokes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Signup(ctx, "r@b.com", "pw12345", "R")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	rotated, err := svc.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == pair.RefreshToken {
		t.Error("refresh token should rotate")
	}
	// Old token must now be rejected (revoked).
	if _, err := svc.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Error("old refresh token must be rejected after rotation")
	}
	// Logout revokes the current one.
	if err := svc.Logout(ctx, rotated.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken); err == nil {
		t.Error("refresh after logout must fail")
	}
}
```

- [ ] **Step 4: Run — expect fail**

Run: `go test ./internal/usecase/auth/`
Expected: FAIL / build error (`Signup` etc. undefined).

- [ ] **Step 5: Implement the service**

Create `internal/usecase/auth/service.go`:
```go
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/password"
	"backend-core/internal/domain"
)

// hashOpaque returns the sha256 hex of an opaque token (refresh tokens are
// stored hashed, never in the clear).
func hashOpaque(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a URL-safe 256-bit random string.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// issuePair issues an access JWT and a fresh rotating refresh token for user.
func (s *Service) issuePair(ctx context.Context, u *domain.User) (*TokenPair, error) {
	access, exp, err := s.d.Tokens.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		return nil, fmt.Errorf("issue access: %w", err)
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	rt := &domain.RefreshToken{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: hashOpaque(refresh),
		ExpiresAt: time.Now().Add(s.d.Config.RefreshTTL),
	}
	if err := s.d.Refresh.Create(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh: %w", err)
	}
	return &TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp}, nil
}

// Signup creates a password user and returns a token pair. ErrAlreadyExists if
// the email is taken.
func (s *Service) Signup(ctx context.Context, email, pw, fullName string) (*TokenPair, error) {
	if email == "" || pw == "" {
		return nil, fmt.Errorf("%w: email and password required", domain.ErrValidation)
	}
	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := s.d.Users.GetByEmail(ctx, email); err == nil {
			return fmt.Errorf("%w: email", domain.ErrAlreadyExists)
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		hash, err := password.Hash(pw)
		if err != nil {
			return err
		}
		u := &domain.User{ID: uuid.New(), Email: &email, FullName: fullName, Role: domain.RoleUser, PreferredLanguage: "ru"}
		if err := s.d.Users.Create(ctx, u); err != nil {
			return err
		}
		if err := s.d.Credentials.Upsert(ctx, &domain.UserCredential{UserID: u.ID, PasswordHash: hash}); err != nil {
			return err
		}
		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Login verifies an email/password and returns a token pair. Returns
// ErrUnauthorized on any mismatch (no user-enumeration signal).
func (s *Service) Login(ctx context.Context, email, pw string) (*TokenPair, error) {
	u, err := s.d.Users.GetByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	cred, err := s.d.Credentials.GetByUserID(ctx, u.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !password.Verify(cred.PasswordHash, pw) {
		return nil, domain.ErrUnauthorized
	}
	return s.issuePair(ctx, u)
}

// Refresh validates a refresh token, rotates it (revokes the old, issues a new
// pair). Rejects unknown, expired, revoked, or reused tokens.
func (s *Service) Refresh(ctx context.Context, refresh string) (*TokenPair, error) {
	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		rt, err := s.d.Refresh.GetByHash(ctx, hashOpaque(refresh))
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil || time.Now().After(rt.ExpiresAt) {
			return domain.ErrUnauthorized
		}
		if err := s.d.Refresh.Revoke(ctx, rt.ID); err != nil {
			return err
		}
		u, err := s.d.Users.GetByID(ctx, rt.UserID)
		if err != nil {
			return err
		}
		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes the given refresh token. Unknown tokens are a no-op success.
func (s *Service) Logout(ctx context.Context, refresh string) error {
	rt, err := s.d.Refresh.GetByHash(ctx, hashOpaque(refresh))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.d.Refresh.Revoke(ctx, rt.ID)
}
```

- [ ] **Step 6: Run — expect pass**

Run: `go test ./internal/usecase/auth/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -w .
git add internal/usecase/auth/
git commit -m "feat(auth): signup, login, refresh rotation, logout usecases"
```

---

## Task 12: Auth usecase — request & verify OTP

**Files:**
- Create: `internal/usecase/auth/otp.go`
- Create: `internal/usecase/auth/otp_test.go`

**Interfaces:**
- Consumes: everything in `Deps`, plus `phone.Normalize`, `otpcode.Generate/Hash`.
- Produces: `RequestOTP(ctx, rawPhone string) (devCode string, err error)` (devCode non-empty only when `Config.OTPDevExpose`); `VerifyOTP(ctx, rawPhone, code string) (*TokenPair, error)`.

- [ ] **Step 1: Write the OTP tests**

Create `internal/usecase/auth/otp_test.go`:
```go
package auth

import (
	"context"
	"testing"
	"time"
)

func TestRequestOTPRateLimit(t *testing.T) {
	svc, sender := newTestService(t)
	ctx := context.Background()

	if _, err := svc.RequestOTP(ctx, "8 707 000 0000"); err != nil {
		t.Fatalf("first RequestOTP: %v", err)
	}
	if sender.lastCode == "" {
		t.Fatal("sender should have received a code")
	}
	// Second within the same minute exceeds OTPPerMin=1.
	if _, err := svc.RequestOTP(ctx, "8 707 000 0000"); err == nil {
		t.Error("expected rate-limit error on immediate second request")
	}
}

func TestVerifyOTPCreatesUserAndIssuesPair(t *testing.T) {
	svc, _ := newTestService(t)
	svc.d.Config.OTPDevExpose = true
	ctx := context.Background()

	code, err := svc.RequestOTP(ctx, "+7 701 111 2222")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if code == "" {
		t.Fatal("dev expose should return the code")
	}
	pair, err := svc.VerifyOTP(ctx, "8 701 111 2222", code) // different formatting, same number
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("expected token pair")
	}
	// A brand-new user now exists for that normalized phone.
	if _, err := svc.d.Users.GetByPhone(ctx, "+77011112222"); err != nil {
		t.Errorf("user should exist for phone: %v", err)
	}
}

func TestVerifyOTPWrongCode(t *testing.T) {
	svc, _ := newTestService(t)
	svc.d.Config.OTPDevExpose = true
	ctx := context.Background()
	if _, err := svc.RequestOTP(ctx, "+7 705 000 0000"); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := svc.VerifyOTP(ctx, "+7 705 000 0000", "000000"); err == nil {
		t.Error("expected error on wrong code")
	}
	_ = time.Now
}
```

- [ ] **Step 2: Run — expect fail**

Run: `go test ./internal/usecase/auth/ -run OTP`
Expected: FAIL / build error (`RequestOTP` undefined).

- [ ] **Step 3: Implement OTP usecases**

Create `internal/usecase/auth/otp.go`:
```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

const maxOTPAttempts = 5

// RequestOTP normalizes the phone, enforces rate limits, stores a hashed code,
// and asks the sender to deliver it. Returns the code only when OTPDevExpose.
func (s *Service) RequestOTP(ctx context.Context, rawPhone string) (string, error) {
	p := phone.Normalize(rawPhone)
	if p == "" {
		return "", fmt.Errorf("%w: phone required", domain.ErrValidation)
	}

	perMin, err := s.d.OTP.CountSince(ctx, p, time.Now().Add(-time.Minute))
	if err != nil {
		return "", err
	}
	if perMin >= s.d.Config.OTPPerMin {
		return "", fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation)
	}
	perHour, err := s.d.OTP.CountSince(ctx, p, time.Now().Add(-time.Hour))
	if err != nil {
		return "", err
	}
	if perHour >= s.d.Config.OTPPerHour {
		return "", fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation)
	}

	code, err := otpcode.Generate()
	if err != nil {
		return "", err
	}
	channel, err := s.d.OTPSender.Send(ctx, p, code)
	if err != nil {
		return "", fmt.Errorf("send otp: %w", err)
	}
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   channel,
		ExpiresAt: time.Now().Add(s.d.Config.OTPTTL),
	}
	if err := s.d.OTP.Create(ctx, rec); err != nil {
		return "", err
	}
	if s.d.Config.OTPDevExpose {
		return code, nil
	}
	return "", nil
}

// VerifyOTP checks the latest active code for the phone; on success it marks the
// code used, finds-or-creates the user, and returns a token pair.
func (s *Service) VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error) {
	p := phone.Normalize(rawPhone)
	if p == "" || code == "" {
		return nil, fmt.Errorf("%w: phone and code required", domain.ErrValidation)
	}

	var pair *TokenPair
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		rec, err := s.d.OTP.LatestActiveByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if rec.Attempts >= maxOTPAttempts {
			return domain.ErrUnauthorized
		}
		if otpcode.Hash(code) != rec.CodeHash {
			_ = s.d.OTP.IncrementAttempts(ctx, rec.ID)
			return domain.ErrUnauthorized
		}
		if err := s.d.OTP.MarkUsed(ctx, rec.ID); err != nil {
			return err
		}

		u, err := s.d.Users.GetByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			now := time.Now()
			u = &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, PreferredLanguage: "ru", PhoneVerifiedAt: &now}
			if err := s.d.Users.Create(ctx, u); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if u.PhoneVerifiedAt == nil {
			now := time.Now()
			u.PhoneVerifiedAt = &now
			if err := s.d.Users.Update(ctx, u); err != nil {
				return err
			}
		}

		pair, err = s.issuePair(ctx, u)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}
```

- [ ] **Step 4: Run — expect pass**

Run: `go test ./internal/usecase/auth/`
Expected: PASS (all auth tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add internal/usecase/auth/otp.go internal/usecase/auth/otp_test.go
git commit -m "feat(auth): phone OTP request (rate-limited) and verify usecases"
```

---

## Task 13: Users usecase — me & update

**Files:**
- Create: `internal/usecase/users/facade.go`
- Create: `internal/usecase/users/facade_test.go`

**Interfaces:**
- Consumes: `domain.UserRepository`.
- Produces: `users.NewFacade(repo domain.UserRepository) *Facade`; `Me(ctx, id uuid.UUID) (*domain.User, error)`; `UpdateMe(ctx, id uuid.UUID, in UpdateInput) (*domain.User, error)` where `UpdateInput{ FullName, AvatarURL, PreferredLanguage, City *string }` (nil = leave unchanged).

- [ ] **Step 1: Write the test**

Create `internal/usecase/users/facade_test.go`:
```go
package users

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type memUsers struct{ m map[uuid.UUID]*domain.User }

func (f *memUsers) Create(_ context.Context, u *domain.User) error { f.m[u.ID] = u; return nil }
func (f *memUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.m[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}
func (f *memUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f *memUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f *memUsers) Update(_ context.Context, u *domain.User) error {
	if _, ok := f.m[u.ID]; !ok {
		return domain.ErrNotFound
	}
	f.m[u.ID] = u
	return nil
}

func strp(s string) *string { return &s }

func TestMeAndUpdate(t *testing.T) {
	id := uuid.New()
	repo := &memUsers{m: map[uuid.UUID]*domain.User{
		id: {ID: id, FullName: "Old", Role: domain.RoleUser, PreferredLanguage: "ru"},
	}}
	f := NewFacade(repo)
	ctx := context.Background()

	got, err := f.Me(ctx, id)
	if err != nil || got.FullName != "Old" {
		t.Fatalf("Me = %+v, %v", got, err)
	}

	updated, err := f.UpdateMe(ctx, id, UpdateInput{FullName: strp("New"), City: strp("Almaty")})
	if err != nil {
		t.Fatalf("UpdateMe: %v", err)
	}
	if updated.FullName != "New" || updated.City == nil || *updated.City != "Almaty" {
		t.Errorf("update not applied: %+v", updated)
	}
	if updated.PreferredLanguage != "ru" {
		t.Errorf("nil field should be unchanged, got %q", updated.PreferredLanguage)
	}

	if _, err := f.Me(ctx, uuid.New()); err == nil {
		t.Error("expected ErrNotFound for unknown user")
	}
}
```

- [ ] **Step 2: Run — expect fail**

Run: `go test ./internal/usecase/users/`
Expected: FAIL / build error (`NewFacade` undefined).

- [ ] **Step 3: Implement the facade**

Create `internal/usecase/users/facade.go`:
```go
// Package users is the application logic for reading and updating the current
// user's profile.
package users

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes profile read/update operations.
type Facade struct{ users domain.UserRepository }

func NewFacade(repo domain.UserRepository) *Facade { return &Facade{users: repo} }

// UpdateInput carries the mutable profile fields. A nil pointer leaves the
// existing value unchanged.
type UpdateInput struct {
	FullName          *string
	AvatarURL         *string
	PreferredLanguage *string
	City              *string
}

// Me returns the user by id, or ErrNotFound.
func (f *Facade) Me(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return f.users.GetByID(ctx, id)
}

// UpdateMe applies the non-nil fields of in and returns the updated user.
func (f *Facade) UpdateMe(ctx context.Context, id uuid.UUID, in UpdateInput) (*domain.User, error) {
	u, err := f.users.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.FullName != nil {
		u.FullName = *in.FullName
	}
	if in.AvatarURL != nil {
		u.AvatarURL = in.AvatarURL
	}
	if in.PreferredLanguage != nil {
		u.PreferredLanguage = *in.PreferredLanguage
	}
	if in.City != nil {
		u.City = in.City
	}
	if err := f.users.Update(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}
```

- [ ] **Step 4: Run — expect pass**

Run: `go test ./internal/usecase/users/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add internal/usecase/users/
git commit -m "feat(users): me and update-profile usecases"
```

---

## Task 14: Auth middleware + Gin app + dependency wiring

**Files:**
- Create: `internal/transport/rest/middleware/auth.go`
- Create: `internal/bootstrap/deps.go`
- Create: `internal/bootstrap/app.go`
- Modify: `cmd/http/main.go`

**Interfaces:**
- Consumes: `auth.TokenIssuer`, `domain.UserRepository`, `auth.Service`, `users.Facade`, `token.RSAIssuer`.
- Produces:
  - `middleware.AuthUser{ ID uuid.UUID; Role string }`; `middleware.Auth(issuer auth.TokenIssuer) gin.HandlerFunc`; `middleware.GetAuthUser(c *gin.Context) (AuthUser, bool)`.
  - `bootstrap.NewDeps(cfg Config, db *sql.DB, log *slog.Logger) (*Deps, error)` exposing `AuthService *auth.Service`, `UsersFacade *users.Facade`, `Issuer *token.RSAIssuer`.
  - `bootstrap.NewApp(cfg Config, deps *Deps, log *slog.Logger) *gin.Engine`; `bootstrap.Run(cfg Config) error`.

- [ ] **Step 1: Implement the auth middleware**

Create `internal/transport/rest/middleware/auth.go`:
```go
// Package middleware holds shared Gin middleware.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/transport/rest/response"
	"backend-core/internal/usecase/auth"
)

type ctxKey struct{}

// AuthUser is the authenticated principal stored on the request context.
type AuthUser struct {
	ID   uuid.UUID
	Role string
}

// Auth verifies the Bearer access token and stores an AuthUser on the context.
// Rejects missing/invalid tokens with 401.
func Auth(issuer auth.TokenIssuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			response.Error(c.Writer, http.StatusUnauthorized, "missing bearer token")
			c.Abort()
			return
		}
		id, role, err := issuer.ParseAccess(strings.TrimSpace(h[7:]))
		if err != nil {
			response.Error(c.Writer, http.StatusUnauthorized, "invalid token")
			c.Abort()
			return
		}
		c.Set(ctxKeyString, AuthUser{ID: id, Role: role})
		c.Next()
	}
}

const ctxKeyString = "auth_user"

// GetAuthUser returns the AuthUser set by Auth.
func GetAuthUser(c *gin.Context) (AuthUser, bool) {
	v, ok := c.Get(ctxKeyString)
	if !ok {
		return AuthUser{}, false
	}
	au, ok := v.(AuthUser)
	return au, ok
}
```

- [ ] **Step 2: Wire dependencies**

Create `internal/bootstrap/deps.go`:
```go
package bootstrap

import (
	"database/sql"
	"fmt"
	"log/slog"

	"backend-core/internal/infrastructure/otpsender"
	otprepo "backend-core/internal/infrastructure/postgres/otp"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	rtrepo "backend-core/internal/infrastructure/postgres/refreshtoken"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/users"
)

// Deps holds the constructed usecases and shared infrastructure.
type Deps struct {
	AuthService *auth.Service
	UsersFacade *users.Facade
	Issuer      *token.RSAIssuer
}

// NewDeps constructs repositories, infrastructure clients, and usecases.
func NewDeps(cfg Config, db *sql.DB, log *slog.Logger) (*Deps, error) {
	issuer, err := token.NewRSAIssuer(cfg.Auth.JWTPrivateKeyPEM, cfg.Auth.JWTKeyID, cfg.Auth.AccessTokenTTL)
	if err != nil {
		return nil, fmt.Errorf("build token issuer: %w", err)
	}
	txm := sqltx.NewManager(db)

	usersRepo := userrepo.New(db)
	credsRepo := credrepo.New(db)
	refreshRepo := rtrepo.New(db)
	otpRepo := otprepo.New(db)

	authSvc := auth.NewService(auth.Deps{
		Users:       usersRepo,
		Credentials: credsRepo,
		Refresh:     refreshRepo,
		OTP:         otpRepo,
		Tx:          txm,
		Tokens:      issuer,
		OTPSender:   otpsender.NewStub(log),
		Config: auth.Config{
			RefreshTTL:   cfg.Auth.RefreshTokenTTL,
			OTPTTL:       cfg.Auth.OTPCodeTTL,
			OTPPerMin:    cfg.Auth.OTPRateLimitPerMin,
			OTPPerHour:   cfg.Auth.OTPRateLimitPerHour,
			OTPDevExpose: cfg.Auth.OTPDevExpose,
		},
	})

	return &Deps{
		AuthService: authSvc,
		UsersFacade: users.NewFacade(usersRepo),
		Issuer:      issuer,
	}, nil
}
```

Note: repositories are constructed with the `*sql.DB` pool; `sqltx.From` swaps in the active tx per request when a usecase runs inside `WithinTx`.

- [ ] **Step 3: Build the Gin app + Run**

Create `internal/bootstrap/app.go`:
```go
package bootstrap

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	authrest "backend-core/internal/transport/rest/auth"
	"backend-core/internal/transport/rest/middleware"
	usersrest "backend-core/internal/transport/rest/users"
)

// NewApp builds the Gin engine with all routes wired.
func NewApp(cfg Config, deps *Deps, log *slog.Logger) *gin.Engine {
	if cfg.App.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}}) })
	r.GET("/.well-known/jwks.json", func(c *gin.Context) { c.JSON(http.StatusOK, deps.Issuer.JWKS()) })

	api := r.Group("/api/v1")
	authrest.NewHandler(deps.AuthService).RegisterRoutes(api)

	authed := api.Group("")
	authed.Use(middleware.Auth(deps.Issuer))
	usersrest.NewHandler(deps.UsersFacade).RegisterRoutes(authed)

	return r
}

// Run loads config, connects the DB, wires deps, and serves HTTP.
func Run(cfg Config, log *slog.Logger) error {
	db, err := NewDB(cfg.DB.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()

	deps, err := NewDeps(cfg, db, log)
	if err != nil {
		return err
	}
	app := NewApp(cfg, deps, log)
	log.Info("http server starting", slog.String("addr", cfg.App.URL))
	return app.Run(cfg.App.URL)
}
```

- [ ] **Step 4: Update the HTTP entry point**

Replace `cmd/http/main.go`:
```go
package main

import (
	"log/slog"
	"os"

	"backend-core/internal/bootstrap"
	"backend-core/internal/logger"
)

func main() {
	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log := logger.New(cfg.App.LogLevel)
	if err := bootstrap.Run(cfg, log); err != nil {
		log.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Build (transport handlers arrive in Tasks 15–16)**

The app references `authrest.NewHandler` and `usersrest.NewHandler`, implemented next. This task does not build alone; verification happens at the end of Task 16. For now:

Run: `go build ./internal/transport/rest/middleware/ ./internal/bootstrap/... 2>&1 | head`
Expected: errors only about the missing `authrest`/`usersrest` packages (proves middleware + deps compile).

- [ ] **Step 6: Commit**

```bash
gofmt -w .
git add internal/transport/rest/middleware/ internal/bootstrap/deps.go internal/bootstrap/app.go cmd/http/main.go
git commit -m "feat(bootstrap): auth middleware, deps wiring, gin app skeleton"
```

---

## Task 15: Auth transport handlers + JWKS

**Files:**
- Create: `internal/transport/rest/auth/request.go`
- Create: `internal/transport/rest/auth/response.go`
- Create: `internal/transport/rest/auth/handler.go`

**Interfaces:**
- Consumes: `auth.Service` methods, `response.Envelope/HandleError`.
- Produces: `authrest.NewHandler(svc *auth.Service) *Handler`; `(*Handler).RegisterRoutes(rg *gin.RouterGroup)` registering `POST auth/signup|login|otp/request|otp/verify|refresh|logout`.

- [ ] **Step 1: Request DTOs**

Create `internal/transport/rest/auth/request.go`:
```go
package auth

// Input DTOs for the auth endpoints. Binding tags drive Gin's validator.

type signupRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
	FullName string `json:"full_name"`
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type otpRequestRequest struct {
	Phone string `json:"phone" binding:"required"`
}

type otpVerifyRequest struct {
	Phone string `json:"phone" binding:"required"`
	Code  string `json:"code" binding:"required"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}
```

- [ ] **Step 2: Response DTOs**

Create `internal/transport/rest/auth/response.go`:
```go
package auth

import (
	"time"

	uc "backend-core/internal/usecase/auth"
)

type tokenPairResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func fromPair(p *uc.TokenPair) tokenPairResponse {
	return tokenPairResponse{AccessToken: p.AccessToken, RefreshToken: p.RefreshToken, ExpiresAt: p.ExpiresAt}
}

type otpRequestedResponse struct {
	Sent bool   `json:"sent"`
	Code string `json:"code,omitempty"` // populated only when AUTH_OTP_DEV_EXPOSE=true
}
```

- [ ] **Step 3: Handler**

Create `internal/transport/rest/auth/handler.go`:
```go
// Package auth exposes the authentication HTTP endpoints.
package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/auth"
)

type Handler struct{ svc *uc.Service }

func NewHandler(svc *uc.Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/auth")
	g.POST("/signup", h.signup)
	g.POST("/login", h.login)
	g.POST("/otp/request", h.otpRequest)
	g.POST("/otp/verify", h.otpVerify)
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
}

func (h *Handler) signup(c *gin.Context) {
	var req signupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.svc.Signup(c.Request.Context(), req.Email, req.Password, req.FullName)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, fromPair(pair))
}

func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.svc.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

func (h *Handler) otpRequest(c *gin.Context) {
	var req otpRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	code, err := h.svc.RequestOTP(c.Request.Context(), req.Phone)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, otpRequestedResponse{Sent: true, Code: code})
}

func (h *Handler) otpVerify(c *gin.Context) {
	var req otpVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.svc.VerifyOTP(c.Request.Context(), req.Phone, req.Code)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

func (h *Handler) refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

func (h *Handler) logout(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.svc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"ok": true})
}
```

- [ ] **Step 4: Verify build of the auth transport package**

Run: `go build ./internal/transport/rest/auth/`
Expected: builds cleanly.

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add internal/transport/rest/auth/
git commit -m "feat(transport): auth HTTP handlers (signup/login/otp/refresh/logout)"
```

---

## Task 16: Users transport handlers + end-to-end build

**Files:**
- Create: `internal/transport/rest/users/request.go`
- Create: `internal/transport/rest/users/response.go`
- Create: `internal/transport/rest/users/handler.go`

**Interfaces:**
- Consumes: `users.Facade`, `middleware.GetAuthUser`, `response.*`.
- Produces: `usersrest.NewHandler(f *users.Facade) *Handler`; `RegisterRoutes` on an already-authenticated group registering `GET users/me` and `PATCH users/me`.

- [ ] **Step 1: Request DTO**

Create `internal/transport/rest/users/request.go`:
```go
package users

import uc "backend-core/internal/usecase/users"

type updateMeRequest struct {
	FullName          *string `json:"full_name"`
	AvatarURL         *string `json:"avatar_url"`
	PreferredLanguage *string `json:"preferred_language"`
	City              *string `json:"city"`
}

func (r updateMeRequest) toInput() uc.UpdateInput {
	return uc.UpdateInput{
		FullName:          r.FullName,
		AvatarURL:         r.AvatarURL,
		PreferredLanguage: r.PreferredLanguage,
		City:              r.City,
	}
}
```

- [ ] **Step 2: Response DTO**

Create `internal/transport/rest/users/response.go`:
```go
package users

import (
	"time"

	"backend-core/internal/domain"
)

type userResponse struct {
	ID                string     `json:"id"`
	Email             *string    `json:"email"`
	Phone             *string    `json:"phone"`
	FullName          string     `json:"full_name"`
	Role              string     `json:"role"`
	AvatarURL         *string    `json:"avatar_url"`
	PreferredLanguage string     `json:"preferred_language"`
	City              *string    `json:"city"`
	EmailVerifiedAt   *time.Time `json:"email_verified_at"`
	PhoneVerifiedAt   *time.Time `json:"phone_verified_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

func fromDomain(u *domain.User) userResponse {
	return userResponse{
		ID: u.ID.String(), Email: u.Email, Phone: u.Phone, FullName: u.FullName,
		Role: string(u.Role), AvatarURL: u.AvatarURL, PreferredLanguage: u.PreferredLanguage,
		City: u.City, EmailVerifiedAt: u.EmailVerifiedAt, PhoneVerifiedAt: u.PhoneVerifiedAt,
		CreatedAt: u.CreatedAt,
	}
}
```

- [ ] **Step 3: Handler**

Create `internal/transport/rest/users/handler.go`:
```go
// Package users exposes the current-user profile HTTP endpoints. Routes must be
// registered on a group already protected by middleware.Auth.
package users

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/users"
)

type Handler struct{ facade *uc.Facade }

func NewHandler(f *uc.Facade) *Handler { return &Handler{facade: f} }

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/users")
	g.GET("/me", h.me)
	g.PATCH("/me", h.updateMe)
}

func (h *Handler) me(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := h.facade.Me(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u))
}

func (h *Handler) updateMe(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c)
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req updateMeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	u, err := h.facade.UpdateMe(c.Request.Context(), au.ID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u))
}
```

- [ ] **Step 4: Full build + unit suite**

Run: `go build ./... && go vet ./... && go test -short ./...`
Expected: builds clean; all unit tests pass (integration tests skipped under `-short`).

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add internal/transport/rest/users/
git commit -m "feat(transport): users /me GET and PATCH handlers; app builds end-to-end"
```

---

## Task 17: ETL command — Supabase dump → clean schema

**Files:**
- Create: `cmd/etl/main.go`
- Create: `cmd/etl/README.md`

**Interfaces:**
- Consumes: `bootstrap.NewConfig`, `bootstrap.NewDB`, `phone.Normalize`.
- Produces: CLI `go run ./cmd/etl/main.go` that reads `raw_supabase.users` (auth) + `raw_supabase.profiles` and upserts into the clean `users` + `user_credentials`.

**Prerequisite (documented, run by the operator, not the plan):** load the dump into a staging schema:
```bash
psql "$DATABASE_URL" -c "CREATE SCHEMA IF NOT EXISTS raw_supabase;"
# Restore the Supabase dump so auth.users lands as raw_supabase.users and
# public.profiles as raw_supabase.profiles (adjust with sed/search_path to taste).
```

- [ ] **Step 1: Write the ETL runner**

Create `cmd/etl/main.go`:
```go
// Command etl performs the one-time migration of identity data from a Supabase
// dump (loaded into the raw_supabase staging schema) into the clean users and
// user_credentials tables. It is idempotent — re-running upserts by id.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"

	"backend-core/internal/auth/phone"
	"backend-core/internal/bootstrap"
	"backend-core/internal/logger"
)

func main() {
	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log := logger.New(cfg.App.LogLevel)
	db, err := bootstrap.NewDB(cfg.DB.Postgres)
	if err != nil {
		log.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	if err := run(context.Background(), db, log); err != nil {
		log.Error("etl failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("etl complete")
}

// run reads staged rows and upserts clean records. It joins auth users with
// profiles on id, preferring profile values where present.
func run(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	const q = `
		SELECT au.id::text,
		       au.email,
		       COALESCE(p.phone, au.raw_user_meta_data->>'phone')          AS phone,
		       COALESCE(p.full_name, au.raw_user_meta_data->>'full_name','') AS full_name,
		       COALESCE(p.role, 'user')                                     AS role,
		       p.avatar_url,
		       COALESCE(p.preferred_language, 'ru')                         AS preferred_language,
		       p.city,
		       au.encrypted_password
		FROM raw_supabase.users au
		LEFT JOIN raw_supabase.profiles p ON p.id = au.id`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	var migrated, withPassword int
	for rows.Next() {
		var (
			id, fullName, role, lang string
			email, rawPhone, avatar, city, encPw sql.NullString
		)
		if err := rows.Scan(&id, &email, &rawPhone, &fullName, &role, &avatar, &lang, &city, &encPw); err != nil {
			return err
		}

		var phonePtr any
		if rawPhone.Valid && rawPhone.String != "" {
			phonePtr = phone.Normalize(rawPhone.String)
		}

		const upsertUser = `
			INSERT INTO users (id, email, phone, full_name, role, avatar_url, preferred_language, city, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now(), now())
			ON CONFLICT (id) DO UPDATE SET
			  email=EXCLUDED.email, phone=EXCLUDED.phone, full_name=EXCLUDED.full_name,
			  role=EXCLUDED.role, avatar_url=EXCLUDED.avatar_url,
			  preferred_language=EXCLUDED.preferred_language, city=EXCLUDED.city,
			  updated_at=now()`
		if _, err := db.ExecContext(ctx, upsertUser,
			id, nullStr(email), phonePtr, fullName, role, nullStr(avatar), lang, nullStr(city),
		); err != nil {
			return err
		}

		if encPw.Valid && encPw.String != "" {
			const upsertCred = `
				INSERT INTO user_credentials (user_id, password_hash) VALUES ($1,$2)
				ON CONFLICT (user_id) DO UPDATE SET password_hash=EXCLUDED.password_hash`
			if _, err := db.ExecContext(ctx, upsertCred, id, encPw.String); err != nil {
				return err
			}
			withPassword++
		}
		migrated++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if migrated == 0 {
		return errors.New("no rows found in raw_supabase — is the dump loaded?")
	}
	log.Info("etl summary", slog.Int("users", migrated), slog.Int("with_password", withPassword))
	return nil
}

// nullStr converts a sql.NullString to any (nil when invalid) for parameters.
func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
```

- [ ] **Step 2: Document the ETL**

Create `cmd/etl/README.md`:
```markdown
# ETL: Supabase dump → clean users domain

One-time, idempotent migration of identity data.

## Steps

1. Migrate the clean schema: `go run ./cmd/migrate/migrate.go up`
2. Create staging schema and load the Supabase dump so that `auth.users` →
   `raw_supabase.users` and `public.profiles` → `raw_supabase.profiles`.
3. Run: `go run ./cmd/etl/main.go`

The command joins auth users with profiles on `id`, preserves the original
UUID, normalizes phones to E.164 (`+7` default), and copies bcrypt password
hashes verbatim so existing passwords keep working. Re-running is safe (upsert
by `id`).
```

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/etl/`
Expected: builds. (A live run requires a loaded dump — an operator step.)

- [ ] **Step 4: Commit**

```bash
gofmt -w .
git add cmd/etl/
git commit -m "feat(etl): one-time idempotent Supabase dump -> users migration"
```

---

## Task 18: End-to-end HTTP flow test, request collection, docs

**Files:**
- Create: `internal/bootstrap/app_test.go`
- Create: `docs/http/auth.http`
- Modify: `Makefile`
- Modify: root `../CLAUDE.md` (workspace file) — add the backend commands note; and `backend-core/CLAUDE.md` — add new commands.

**Interfaces:**
- Consumes: `bootstrap.NewApp`, `bootstrap.NewDeps`, `token.GenerateTestKeyPEM`, `testdb`.

- [ ] **Step 1: Write the end-to-end HTTP test**

Create `internal/bootstrap/app_test.go`:
```go
package bootstrap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/logger"
)

// buildTestApp wires a real app against the test DB with a fresh signing key and
// OTP dev-expose enabled.
func buildTestApp(t *testing.T) http.Handler {
	t.Helper()
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users", "otp_codes", "refresh_tokens")
	log := logger.New("error")
	cfg := Config{}
	cfg.App.Environment = "test"
	cfg.Auth = AuthConfig{
		JWTPrivateKeyPEM: token.GenerateTestKeyPEM(t),
		JWTKeyID:         "test",
		AccessTokenTTL:   15 * time.Minute,
		RefreshTokenTTL:  time.Hour,
		OTPCodeTTL:       5 * time.Minute,
		OTPRateLimitPerMin:  5,
		OTPRateLimitPerHour: 20,
		OTPDevExpose:     true,
	}
	deps, err := NewDeps(cfg, db, log)
	if err != nil {
		t.Fatalf("NewDeps: %v", err)
	}
	return NewApp(cfg, deps, log)
}

func doJSON(t *testing.T, app http.Handler, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestSignupLoginMeFlow(t *testing.T) {
	app := buildTestApp(t)

	rec, out := doJSON(t, app, "POST", "/api/v1/auth/signup", "", map[string]string{
		"email": "e2e@bookeat.com", "password": "pw123456", "full_name": "E2E",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup status %d: %v", rec.Code, out)
	}
	data := out["data"].(map[string]any)
	access := data["access_token"].(string)

	rec, out = doJSON(t, app, "GET", "/api/v1/users/me", access, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status %d: %v", rec.Code, out)
	}
	me := out["data"].(map[string]any)
	if me["email"] != "e2e@bookeat.com" || me["full_name"] != "E2E" {
		t.Errorf("unexpected /me: %v", me)
	}

	// Unauthenticated /me is rejected.
	rec, _ = doJSON(t, app, "GET", "/api/v1/users/me", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rec.Code)
	}
}

func TestOTPFlow(t *testing.T) {
	app := buildTestApp(t)

	rec, out := doJSON(t, app, "POST", "/api/v1/auth/otp/request", "", map[string]string{"phone": "8 701 555 0000"})
	if rec.Code != http.StatusOK {
		t.Fatalf("otp request status %d: %v", rec.Code, out)
	}
	code := out["data"].(map[string]any)["code"].(string)

	rec, out = doJSON(t, app, "POST", "/api/v1/auth/otp/verify", "", map[string]string{
		"phone": "+7 701 555 0000", "code": code,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("otp verify status %d: %v", rec.Code, out)
	}
	if out["data"].(map[string]any)["access_token"].(string) == "" {
		t.Error("expected access token from otp verify")
	}
}
```

- [ ] **Step 2: Run the e2e test (with test DB)**

Run: `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./internal/bootstrap/`
Expected: PASS. (Skips cleanly under `go test -short`.)

- [ ] **Step 3: Manual request collection**

Create `docs/http/auth.http`:
```http
@base = http://localhost:8080

### Signup
POST {{base}}/api/v1/auth/signup
Content-Type: application/json

{ "email": "you@example.com", "password": "pw123456", "full_name": "You" }

### Login
POST {{base}}/api/v1/auth/login
Content-Type: application/json

{ "email": "you@example.com", "password": "pw123456" }

### Request OTP (code is logged; echoed in response when AUTH_OTP_DEV_EXPOSE=true)
POST {{base}}/api/v1/auth/otp/request
Content-Type: application/json

{ "phone": "8 701 555 0000" }

### Verify OTP
POST {{base}}/api/v1/auth/otp/verify
Content-Type: application/json

{ "phone": "+77015550000", "code": "123456" }

### Refresh
POST {{base}}/api/v1/auth/refresh
Content-Type: application/json

{ "refresh_token": "PASTE_REFRESH" }

### Me
GET {{base}}/api/v1/users/me
Authorization: Bearer PASTE_ACCESS

### JWKS
GET {{base}}/.well-known/jwks.json
```

- [ ] **Step 4: Update the Makefile**

Add these targets to `Makefile` (append; keep `.PHONY` in sync):
```makefile
etl:
	go run ./cmd/etl/main.go

# Integration tests need a migrated Postgres; point TEST_DATABASE_URL at it.
test-integration:
	go test ./...
```
Update the `mocks` target comment is unnecessary; leave it. Add `etl test-integration` to the `.PHONY` line.

- [ ] **Step 5: Update docs**

In `backend-core/CLAUDE.md`, under Commands, add:
```
go run ./cmd/etl/main.go        # one-time Supabase dump → users ETL (see cmd/etl/README.md)
TEST_DATABASE_URL=... go test ./...   # integration tests (need a migrated Postgres)
```
In the root workspace `CLAUDE.md` (`../CLAUDE.md`), append a line to the backend section noting that auth + users now live in `backend-core` behind `/api/v1/auth/*` and `/api/v1/users/me`, JWT is RS256 with a JWKS endpoint, and the frontend has not yet been cut over.

- [ ] **Step 6: Final full verification**

Run:
```bash
gofmt -w . && go vet ./...
go test -short ./...
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/bookeat?sslmode=disable go test ./...
```
Expected: vet clean; `-short` passes with integration skipped; full run passes with a test DB.

- [ ] **Step 7: Commit**

```bash
git add internal/bootstrap/app_test.go docs/http/auth.http Makefile CLAUDE.md ../CLAUDE.md
git commit -m "test(e2e): signup/login/me + otp flow; http collection and docs"
```

---

## Self-Review notes (for the executor)

- **Spec coverage:** users domain (Task 5), clean schema (Task 4), ETL preserving UUID + bcrypt + E.164 (Task 17), email/password (Tasks 11, 15), phone-OTP with rate limits + stub delivery (Tasks 10, 12, 15), RS256 JWT + JWKS (Tasks 7, 14), refresh rotation (Task 11), `/me` (Tasks 13, 16), verification via API tests + HTTP collection (Tasks 8–9, 18), frontend untouched (no frontend files in any task). Google OAuth and live OTP providers intentionally absent (out of scope).
- **Type consistency:** repo constructors are all `New(pool sqltx.DBTX)`; `auth.Deps` field names match `NewService`/`NewDeps`; `TokenIssuer`/`OTPSender` method sets match `token.RSAIssuer` and `otpsender.Stub`; `users.UpdateInput` fields match the transport DTO mapper.
- **Known integration caveat:** Task 14 does not build standalone (references handlers from Tasks 15–16); the first clean full build is Task 16 Step 4. This is called out in Task 14 Step 5.
```
