# Design: Extract `users` + `auth` domain from Supabase into `backend-core`

**Date:** 2026-07-09
**Status:** Approved (design), pending implementation plan
**Scope:** First slice of the gradual Supabase → Go backend extraction.

## Context

BookEat currently runs entirely on Supabase (Postgres + Supabase Auth + Deno edge
functions), consumed directly by the `book-eat-app` frontend. We are incrementally
extracting the backend into `backend-core` (Go, Clean/Hexagonal — see
`backend-core/CLAUDE.md`), one domain at a time (strangler-fig).

This first slice extracts **identity**: the `users` domain and **authentication**.
Identity goes first because every other domain (bookings, loyalty, restaurants)
references `user_id`; preserving the same user UUID keeps every later slice
straightforward.

### Fixed decisions (from brainstorming)

1. **Auth model:** `backend-core` becomes its own identity provider and **issues its
   own JWTs**, fully replacing Supabase Auth as the end state.
2. **Schema:** clean domain schema authored via goose migrations + a one-time ETL
   from a Supabase SQL dump. The Go domain has **no knowledge of Supabase**.
3. **First-slice login methods:** phone-OTP + email/password. **Google OAuth is out
   of scope** for this slice.
4. **Verification:** API-level tests (unit + integration) against a local Postgres
   seeded by the ETL, plus an HTTP request collection for manual checks. The
   `book-eat-app` frontend is **not touched** and stays on Supabase.
5. **JWT signing:** **RS256** with a JWKS endpoint.
6. **OTP delivery:** **stubbed** (dev log / dev-only response) in this slice; the real
   provider waterfall is deferred to a later phase.

### Grounding facts (verified in the current codebase/dump)

- Supabase hashes passwords with **bcrypt** (`$2a$10$…` in
  `auth.users.encrypted_password`) → Go's `golang.org/x/crypto/bcrypt` can verify
  imported hashes directly, so **existing users keep their passwords** (no reset).
- `auth.users`: `id (uuid)`, `email`, `encrypted_password`, timestamps,
  `raw_user_meta_data` jsonb (`{full_name, phone}`).
- `public.profiles`: `id (= auth.users.id)`, `full_name`, `phone`, `role`,
  `avatar_url`, `preferred_language` (default `'ru'`), `email`, `city`,
  `created_at`. Role values in use: `admin`, `restaurant`, `user`.
- `public.phone_otp_codes`: `phone`, `code_hash` (sha256), `channel`, `attempts`,
  `used_at`, `expires_at`, `ip_address`, `user_agent`, `created_at`. Rate limits in
  the current edge function: 1/min, 5/hour. Code length 6, TTL 300s.
- Frontend phone normalization (`usePhoneOtpAuth.normalizePhone`) targets **E.164**
  defaulting to `+7` (KZ/RU) — the ETL and OTP flow must match this.

## Non-goals (explicitly out of scope)

- Google OAuth.
- Live OTP delivery providers (Telegram / Telegram Gateway / WhatsApp/Twilio /
  Mobizon SMS).
- Any change to `book-eat-app` (frontend, Android, iOS).
- Any domain other than `users`.
- Ongoing sync with the live Supabase database — the dump is a **one-time snapshot**;
  a real data migration happens at cutover time (future phase).

## Roadmap (phases)

- **Phase 0 — foundation** (one-time, prerequisite): add what `backend-core` lacks
  today — Postgres driver + connection pool, HTTP router (gin, per CLAUDE.md),
  `sqltx.Manager` transaction manager, `middleware.Auth`, goose migration wiring.
  The existing scaffold is already shaped for this.
- **Phase 1 — this slice:** `users` + `auth` domain, ETL, email/password + phone-OTP
  flows, JWT issuance (RS256), `/me`.
- **Phase 2+ (later, not now):** live OTP providers, Google OAuth, frontend cutover
  behind a feature flag, subsequent domains.

## Domain model (clean schema, goose migrations)

One file per entity under `internal/domain/` (`VARCHAR` for enumerated fields, no DB
enums, no frameworks — per CLAUDE.md).

### `users`
| column | type | notes |
| --- | --- | --- |
| `id` | uuid PK | **same UUID as Supabase** `auth.users.id` |
| `email` | varchar unique, nullable | |
| `phone` | varchar unique, nullable | E.164 |
| `full_name` | varchar | |
| `role` | varchar not null default `'user'` | `user` / `restaurant` / `admin` |
| `avatar_url` | varchar nullable | |
| `preferred_language` | varchar not null default `'ru'` | |
| `city` | varchar nullable | |
| `email_verified_at` | timestamptz nullable | |
| `phone_verified_at` | timestamptz nullable | |
| `created_at` | timestamptz not null | |
| `updated_at` | timestamptz not null | |

`profiles` and the needed `auth.users` fields are merged into this single entity.

### `user_credentials`
`user_id` (FK, PK), `password_hash` (bcrypt, imported from Supabase). Kept separate
from `users` so the hash is not read on every profile fetch. Rows exist only for
users who have a password (OTP-only users may have none).

### `otp_codes`
`id`, `phone`, `code_hash` (sha256), `channel` (nullable, always `stub` in this
slice), `attempts int default 0`, `used_at` nullable, `expires_at`, `created_at`.
Indexed on `(phone, created_at desc)` and `expires_at`.

### `refresh_tokens`
`id`, `user_id` (FK), `token_hash`, `expires_at`, `revoked_at` nullable,
`user_agent` nullable, `created_at`. Supports refresh-token rotation.

### Sentinel errors
Reuse `domain` sentinels (`ErrNotFound`, `ErrAlreadyExists`, `ErrUnauthorized`,
`ErrValidation`, …) so `response.HandleError` maps them to HTTP status codes.

## ETL from the dump (one-time, reproducible)

A dedicated `cmd/etl/` entry point (or SQL script). Steps:

1. Load the Supabase dump into a staging schema `raw_supabase`.
2. `INSERT … SELECT` from `raw_supabase.auth.users` + `raw_supabase.public.profiles`
   into the clean `users` + `user_credentials` tables.

Rules:
- **Preserve the UUID** (`users.id = auth.users.id`).
- Merge `raw_user_meta_data.full_name/phone` with `profiles` (profiles wins where
  present; never fabricate a name).
- Copy the bcrypt `encrypted_password` verbatim into `user_credentials.password_hash`
  (only when present).
- Normalize `phone` to E.164 matching the frontend's `normalizePhone` (default `+7`).
- **Idempotent** — re-running does not duplicate rows (upsert on `id`).

Loading via a staging schema (rather than reading the raw dump inline) keeps the ETL
reproducible against a fresh dump.

## Auth flows & tokens

### JWT (RS256)
- Access token: short-lived (~15 min). Claims: `sub` (uuid), `role`, `exp`, `iat`.
- Signed with an RSA private key held by `backend-core` (loaded from config/secret,
  never committed). Public key served at `GET /.well-known/jwks.json` so any service
  can validate without a shared secret.
- `middleware.Auth` verifies the RS256 signature, loads the user, rejects inactive
  users, and stashes `AuthUser{ID, Role}` in the request context.

### Refresh tokens
Opaque random token returned to the client; only its hash is stored in
`refresh_tokens`. **Rotated on every use** (old row revoked, new row issued). Reuse of
a revoked token is rejected.

### Email + password
- `signup`: create `users` + `user_credentials` (bcrypt), issue token pair.
- `login`: verify bcrypt (compatible with imported Supabase hashes), issue token pair.

### Phone OTP
- `request-otp`: normalize phone → generate 6-digit code → store sha256 hash with
  300s TTL → rate-limit **1/min, 5/hour** per phone. Delivery is **stubbed**: the code
  is logged (and, in dev only, may be returned in the response) — no real SMS.
- `verify-otp`: compare sha256(submitted) against the stored hash, enforce attempt
  count/expiry, mark `used_at`. On first successful verification for an unknown phone,
  **create the user** (`role = 'user'`). Issue token pair.

## HTTP API (this slice)

```
POST  /api/v1/auth/signup            email + password → {access, refresh}
POST  /api/v1/auth/login             email + password → {access, refresh}
POST  /api/v1/auth/otp/request       { phone }
POST  /api/v1/auth/otp/verify        { phone, code } → {access, refresh}
POST  /api/v1/auth/refresh           { refresh } → new pair (rotation)
POST  /api/v1/auth/logout            revoke refresh token
GET   /api/v1/users/me               current profile (Bearer)
PATCH /api/v1/users/me               update mutable profile fields
GET   /.well-known/jwks.json         public key (RS256)
GET   /health                        unauthenticated
```

All responses wrapped in `response.Envelope`; all errors routed through
`response.HandleError`.

## Code layout (mapped onto `backend-core` layers)

- `internal/domain/`: `user.go`, `user_credential.go`, `otp.go`, `refresh_token.go`
  (structs + repository interfaces + typed constants).
- `internal/usecase/auth/`: signup, login, otp request/verify, refresh, logout. Declares
  local **ports** for token issuance and OTP delivery (Interface Segregation).
- `internal/usecase/users/`: profile read/update facade.
- `internal/transport/rest/auth/` and `internal/transport/rest/users/`:
  `handler.go` / `request.go` / `response.go` each.
- `internal/infrastructure/postgres/{user,usercredential,otp,refreshtoken}/`: repos
  pulling the active tx via `sqltx.From(ctx)`.
- `internal/infrastructure/token/`: RSA/JWT issuer + verifier implementing the usecase
  token port.
- `internal/infrastructure/otpsender/`: stub sender implementing the OTP-delivery port.
- Wiring in `internal/bootstrap/deps.go`.

## Verification (frontend untouched)

- **Unit:** phone normalization, OTP generate/verify, rate-limit, bcrypt verify,
  JWT issue/parse (RS256), refresh-token rotation.
- **Integration** (non-short suite, real Postgres seeded by ETL): full
  `signup → login → refresh → /me` and `otp request → verify → /me`.
- **HTTP collection** (`.http` / curl) for manual exercise of every endpoint.
- **Compatibility smoke:** a real imported bcrypt hash logs in successfully, and
  imported phones match production values — so a future frontend cutover is clean.

## Risks & mitigations

- **bcrypt cost / algorithm mismatch:** Supabase uses `$2a$` bcrypt; Go's bcrypt
  reads `$2a$`/`$2b$`. Covered by the compatibility smoke test on a real hash.
- **Phone normalization drift** between ETL and the OTP flow → dedupe/lookup misses.
  Mitigation: a single shared normalization function used by both, unit-tested against
  the frontend's cases.
- **Dump/live divergence:** accepted — the dump is a one-time snapshot for building
  and proving the backend; a real migration happens at cutover.
- **RSA key management:** private key from secret/config only, never committed;
  documented in `.env.example` as a placeholder.
