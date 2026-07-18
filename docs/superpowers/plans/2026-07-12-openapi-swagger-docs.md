# OpenAPI / Swagger Documentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Document every HTTP endpoint of `backend-core` as a committed Swagger 2.0 spec with clear descriptions and request/response examples, generated from swaggo annotations.

**Architecture:** Add swaggo/swag declarative comment annotations to the Gin handlers (auth, users) and to a stub file for the inline health/JWKS routes. Generate `docs/swagger.yaml` + `docs/swagger.json` via `swag init` (run through `go run ...@latest`), then delete the generated `docs/docs.go` so no runtime dependency lands in `go.mod`. The spec is file-only — the service does not serve it.

**Tech Stack:** Go 1.25.7, Gin, `github.com/swaggo/swag/cmd/swag@latest` (v1.16.4) as a `go run` tool only.

## Global Constraints

- Go module `backend-core`, Go 1.25.7. Public modules + stdlib only (CLAUDE.md).
- **No new dependency in `go.mod`/`go.sum`.** `swag` runs via `go run github.com/swaggo/swag/cmd/swag@latest`; the generated `docs/docs.go` is deleted after every generation and never committed/imported.
- Generation command (verified against this repo):
  `go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/http/swagger.go -d ./ --parseInternal -o docs && rm -f docs/docs.go`
  (A benign `warning: ... no Go files in ./` line is expected and harmless.)
- Output format is **Swagger 2.0** (`swagger: "2.0"`) — this is what swaggo v1 emits; both editor.swagger.io and Postman consume it.
- Every response is wrapped in `response.Envelope`: `{"data": ...}` on success, `{"error": "..."}` on failure — **except** `/.well-known/jwks.json`, which returns a raw JWKS. Model enveloped success as `response.Envelope{data=<DTO>}`.
- Error status mapping (from `response.HandleError`): 404 not found, 409 already exists, 403 forbidden, 401 unauthorized, 422 validation failed / invalid status, 500 internal. Body-binding failures → 422. Do **not** document statuses the code cannot produce (e.g. no 429).
- Request/response examples come **only** from `example:"..."` struct tags on the DTO fields — no hand-duplicated JSON in annotations.
- swaggo resolves unexported DTO types (`auth.loginRequest`, etc.) correctly — **verified**; no need to export anything.
- Endpoints to cover (11 total): `GET /health`, `GET /health/ready`, `GET /.well-known/jwks.json`, `POST /api/v1/auth/{signup,login,otp/request,otp/verify,refresh,logout}`, `GET /api/v1/users/me`, `PATCH /api/v1/users/me`.

---

## File Structure

- **Create** `cmd/http/swagger.go` — general API info (`@title`, `@version`, `@description`), `BearerAuth` security definition, and three never-called stub functions carrying the swaggo annotations for `/health`, `/health/ready`, `/.well-known/jwks.json`. Keeps `app.go` untouched.
- **Modify** `internal/transport/rest/auth/handler.go` — annotation blocks above the 6 handler methods.
- **Modify** `internal/transport/rest/auth/request.go` — `example` tags on request DTO fields.
- **Modify** `internal/transport/rest/auth/response.go` — `example` tags on `tokenPairResponse`, `otpRequestedResponse`.
- **Modify** `internal/transport/rest/users/handler.go` — annotation blocks above the 2 handler methods (with `@Security BearerAuth`).
- **Modify** `internal/transport/rest/users/request.go` — `example` tags on `updateMeRequest`.
- **Modify** `internal/transport/rest/users/response.go` — `example` tags on `userResponse`.
- **Modify** `Makefile` — add a `swagger` target.
- **Create** (generated, committed) `docs/swagger.yaml`, `docs/swagger.json`.
- **Not committed** `docs/docs.go` — deleted after generation.

Note: `docs/superpowers/` is git-ignored in this repo, but `docs/swagger.{yaml,json}` are **not** — verify they are trackable in Task 4.

---

## Task 1: Generation scaffolding + system (health/JWKS) endpoints

Establishes the general API info, the security scheme, the three system-endpoint annotations, and the `make swagger` workflow. Deliverable: `make swagger` produces a valid `docs/swagger.yaml` containing the three system paths, and `docs/docs.go` is gone.

**Files:**
- Create: `cmd/http/swagger.go`
- Modify: `Makefile`
- Generated: `docs/swagger.yaml`, `docs/swagger.json`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: the general info file `cmd/http/swagger.go` that `swag init -g` points at; later tasks add annotations elsewhere and re-run `make swagger`. Security scheme name is `BearerAuth`.

- [ ] **Step 1: Create `cmd/http/swagger.go`**

```go
package main

// @title       BookEat backend-core API
// @version     1.0
// @description Core backend service for BookEat.
// @description
// @description Every response is wrapped in a JSON envelope — {"data": ...} on
// @description success, {"error": "..."} on failure — except /.well-known/jwks.json,
// @description which returns a raw JWKS document.

// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description Access token from POST /api/v1/auth/login (or signup / otp/verify /
// @description refresh), sent as: Authorization: Bearer <access_token>

// The functions below are never called. They exist only to attach swaggo
// annotations to the health and JWKS routes, which are registered as inline
// closures in internal/bootstrap/app.go and therefore have no handler function
// of their own for swag to read.

// swaggerHealth documents GET /health.
// @Summary     Liveness probe
// @Description Returns 200 while the process is up. Does not check dependencies.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"data\":{\"status\":\"ok\"}}"
// @Router      /health [get]
func swaggerHealth() {}

// swaggerReady documents GET /health/ready.
// @Summary     Readiness probe
// @Description Pings the database. Returns 200 when reachable, 503 otherwise.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"data\":{\"status\":\"ready\"}}"
// @Failure     503 {object} map[string]interface{} "{\"data\":{\"status\":\"unavailable\"}}"
// @Router      /health/ready [get]
func swaggerReady() {}

// swaggerJWKS documents GET /.well-known/jwks.json.
// @Summary     JWKS
// @Description Public RS256 keys used to verify access-token signatures. This
// @Description response is NOT wrapped in the standard envelope.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"keys\":[{\"kty\":\"RSA\",\"use\":\"sig\",\"alg\":\"RS256\",\"kid\":\"...\",\"n\":\"...\",\"e\":\"AQAB\"}]}"
// @Router      /.well-known/jwks.json [get]
func swaggerJWKS() {}
```

- [ ] **Step 2: Verify the package still builds**

Run: `go build ./...`
Expected: exits 0 (unused package-level functions are legal in Go).

- [ ] **Step 3: Add the `swagger` target to `Makefile`**

Add `swagger` to the `.PHONY` line, and append this target at the end of the file:

```makefile
# Regenerate the committed OpenAPI/Swagger spec from swaggo annotations.
# Runs swag as a one-off tool (no go.mod dependency); docs.go is discarded so
# nothing pulls swaggo into the build.
swagger:
	go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/http/swagger.go -d ./ --parseInternal -o docs
	rm -f docs/docs.go
```

- [ ] **Step 4: Generate the spec**

Run: `make swagger`
Expected: prints `create swagger.json at docs/swagger.json` and `create swagger.yaml at docs/swagger.yaml`; a `warning: ... no Go files in ./` line may appear and is harmless.

- [ ] **Step 5: Verify system endpoints and cleanup**

Run: `test ! -f docs/docs.go && grep -c "/health" docs/swagger.yaml && grep -q "jwks.json" docs/swagger.yaml && grep -q "BearerAuth" docs/swagger.yaml && echo OK`
Expected: prints a count ≥ 1 for `/health`, then `OK`. Confirms `docs.go` was removed and the system paths + security scheme are present.

- [ ] **Step 6: Confirm go.mod/go.sum untouched**

Run: `git diff --stat go.mod go.sum`
Expected: no output (no new dependency added).

- [ ] **Step 7: Commit**

```bash
git add cmd/http/swagger.go Makefile docs/swagger.yaml docs/swagger.json
git commit -m "docs(swagger): scaffold spec generation + system endpoints"
```

---

## Task 2: Auth endpoint annotations + examples

Annotate the six `/api/v1/auth/*` handlers and add example tags to their DTOs. Deliverable: regenerated spec contains all six auth paths with enveloped token-pair responses and field examples.

**Files:**
- Modify: `internal/transport/rest/auth/handler.go`
- Modify: `internal/transport/rest/auth/request.go`
- Modify: `internal/transport/rest/auth/response.go`
- Generated: `docs/swagger.yaml`, `docs/swagger.json`

**Interfaces:**
- Consumes: `cmd/http/swagger.go` general info + `BearerAuth` scheme (Task 1); `response.Envelope` composition syntax.
- Produces: definitions `auth.signupRequest`, `auth.loginRequest`, `auth.otpRequestRequest`, `auth.otpVerifyRequest`, `auth.refreshRequest`, `auth.tokenPairResponse`, `auth.otpRequestedResponse` (consumed by no later task, but shared with Task 4 verification).

- [ ] **Step 1: Add example tags in `internal/transport/rest/auth/request.go`**

Replace the file body's struct definitions with:

```go
type signupRequest struct {
	Email    string `json:"email" binding:"required,email" example:"user@example.com"`
	Password string `json:"password" binding:"required,min=6" example:"s3cret123"`
	FullName string `json:"full_name" example:"Jane Doe"`
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email" example:"user@example.com"`
	Password string `json:"password" binding:"required" example:"s3cret123"`
}

type otpRequestRequest struct {
	Phone string `json:"phone" binding:"required" example:"+77011234567"`
}

type otpVerifyRequest struct {
	Phone string `json:"phone" binding:"required" example:"+77011234567"`
	Code  string `json:"code" binding:"required" example:"123456"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required" example:"9c8b7a6f-...-refresh"`
}
```

- [ ] **Step 2: Add example tags in `internal/transport/rest/auth/response.go`**

Change the `tokenPairResponse` and `otpRequestedResponse` struct definitions to:

```go
type tokenPairResponse struct {
	AccessToken  string    `json:"access_token" example:"eyJhbGciOiJSUzI1NiIsImtpZCI6..."`
	RefreshToken string    `json:"refresh_token" example:"9c8b7a6f-4d3e-2b1a-...-refresh"`
	ExpiresAt    time.Time `json:"expires_at" example:"2026-07-12T18:30:00Z"`
}
```

```go
type otpRequestedResponse struct {
	Sent bool   `json:"sent" example:"true"`
	Code string `json:"code,omitempty" example:"123456"` // populated only when AUTH_OTP_DEV_EXPOSE=true
}
```

- [ ] **Step 3: Annotate the six handlers in `internal/transport/rest/auth/handler.go`**

Insert the matching block immediately above each handler's `func (h *Handler) ...` line.

Above `signup`:
```go
// signup registers a new user and returns an access/refresh token pair.
// @Summary     Sign up
// @Description Registers a new user with email + password and returns a token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body signupRequest true "New account details"
// @Success     201 {object} response.Envelope{data=tokenPairResponse}
// @Failure     409 {object} response.Envelope "email already registered"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/signup [post]
```

Above `login`:
```go
// login authenticates a user by email and password.
// @Summary     Log in
// @Description Authenticates by email + password and returns a token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body loginRequest true "Credentials"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid credentials"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/login [post]
```

Above `otpRequest`:
```go
// otpRequest generates and sends a one-time code to a phone number.
// @Summary     Request an OTP code
// @Description Generates a one-time code and delivers it to the phone. Rate-limited
// @Description (per-minute and per-hour); over the limit returns 422. The response
// @Description "code" field is populated only when AUTH_OTP_DEV_EXPOSE=true.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body otpRequestRequest true "Phone number"
// @Success     200 {object} response.Envelope{data=otpRequestedResponse}
// @Failure     422 {object} response.Envelope "validation failed / rate limited"
// @Router      /api/v1/auth/otp/request [post]
```

Above `otpVerify`:
```go
// otpVerify checks a one-time code and returns a token pair on success.
// @Summary     Verify an OTP code
// @Description Verifies the latest active code for the phone. On success, finds or
// @Description creates the user and returns a token pair. Wrong/expired codes and
// @Description too many attempts return 401.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body otpVerifyRequest true "Phone and code"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid or expired code"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/otp/verify [post]
```

Above `refresh`:
```go
// refresh exchanges a refresh token for a new token pair.
// @Summary     Refresh tokens
// @Description Exchanges a valid refresh token for a new access/refresh token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body refreshRequest true "Refresh token"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid or expired refresh token"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/refresh [post]
```

Above `logout`:
```go
// logout revokes a refresh token.
// @Summary     Log out
// @Description Revokes the given refresh token so it can no longer be used.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body refreshRequest true "Refresh token to revoke"
// @Success     200 {object} response.Envelope{data=object} "{\"data\":{\"ok\":true}}"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/logout [post]
```

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 5: Regenerate the spec**

Run: `make swagger`
Expected: `create swagger.yaml at docs/swagger.yaml`.

- [ ] **Step 6: Verify all six auth paths and examples are present**

Run: `for p in signup login otp/request otp/verify refresh logout; do grep -q "/api/v1/auth/$p:" docs/swagger.yaml && echo "have $p" || echo "MISSING $p"; done; grep -q "user@example.com" docs/swagger.yaml && grep -q "auth.tokenPairResponse" docs/swagger.yaml && echo OK`
Expected: `have signup` … `have logout` (all six), then `OK`.

- [ ] **Step 7: Confirm cleanup + go.mod untouched**

Run: `test ! -f docs/docs.go && git diff --stat go.mod go.sum && echo CLEAN`
Expected: no go.mod/go.sum diff, prints `CLEAN`.

- [ ] **Step 8: Commit**

```bash
git add internal/transport/rest/auth/ docs/swagger.yaml docs/swagger.json
git commit -m "docs(swagger): annotate auth endpoints with examples"
```

---

## Task 3: Users endpoint annotations + examples

Annotate the two authenticated `/api/v1/users/me` handlers (with `BearerAuth`) and add example tags. Deliverable: regenerated spec contains both user paths, secured, with a `userResponse` schema carrying examples.

**Files:**
- Modify: `internal/transport/rest/users/handler.go`
- Modify: `internal/transport/rest/users/request.go`
- Modify: `internal/transport/rest/users/response.go`
- Generated: `docs/swagger.yaml`, `docs/swagger.json`

**Interfaces:**
- Consumes: `BearerAuth` security scheme (Task 1).
- Produces: definitions `users.updateMeRequest`, `users.userResponse`.

- [ ] **Step 1: Add example tags in `internal/transport/rest/users/request.go`**

Change the `updateMeRequest` struct to:

```go
type updateMeRequest struct {
	FullName          *string `json:"full_name" example:"Jane Doe"`
	AvatarURL         *string `json:"avatar_url" example:"https://cdn.example.com/a/jane.png"`
	PreferredLanguage *string `json:"preferred_language" example:"ru"`
	City              *string `json:"city" example:"almaty"`
}
```

- [ ] **Step 2: Add example tags in `internal/transport/rest/users/response.go`**

Change the `userResponse` struct to:

```go
type userResponse struct {
	ID                string     `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	Email             *string    `json:"email" example:"user@example.com"`
	Phone             *string    `json:"phone" example:"+77011234567"`
	FullName          string     `json:"full_name" example:"Jane Doe"`
	Role              string     `json:"role" example:"user"`
	AvatarURL         *string    `json:"avatar_url" example:"https://cdn.example.com/a/jane.png"`
	PreferredLanguage string     `json:"preferred_language" example:"ru"`
	City              *string    `json:"city" example:"almaty"`
	EmailVerifiedAt   *time.Time `json:"email_verified_at" example:"2026-07-10T09:00:00Z"`
	PhoneVerifiedAt   *time.Time `json:"phone_verified_at" example:"2026-07-10T09:00:00Z"`
	CreatedAt         time.Time  `json:"created_at" example:"2026-01-15T08:30:00Z"`
}
```

- [ ] **Step 3: Annotate the two handlers in `internal/transport/rest/users/handler.go`**

Above `me`:
```go
// me returns the authenticated user's profile.
// @Summary     Get current user
// @Description Returns the profile of the authenticated user.
// @Tags        users
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope{data=userResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/users/me [get]
```

Above `updateMe`:
```go
// updateMe applies a partial update to the authenticated user's profile.
// @Summary     Update current user
// @Description Partially updates the authenticated user's profile. Only the
// @Description provided fields are changed; omitted fields are left untouched.
// @Tags        users
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body updateMeRequest true "Fields to update (all optional)"
// @Success     200 {object} response.Envelope{data=userResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/users/me [patch]
```

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 5: Regenerate the spec**

Run: `make swagger`
Expected: `create swagger.yaml at docs/swagger.yaml`.

- [ ] **Step 6: Verify user paths, security, and examples**

Run: `grep -q "/api/v1/users/me:" docs/swagger.yaml && grep -q "users.userResponse" docs/swagger.yaml && grep -q "BearerAuth: \[\]" docs/swagger.yaml && grep -q "550e8400-e29b-41d4-a716-446655440000" docs/swagger.yaml && echo OK`
Expected: `OK` (path present, schema present, security applied to an operation, example rendered).

- [ ] **Step 7: Confirm cleanup + go.mod untouched**

Run: `test ! -f docs/docs.go && git diff --stat go.mod go.sum && echo CLEAN`
Expected: no go.mod/go.sum diff, prints `CLEAN`.

- [ ] **Step 8: Commit**

```bash
git add internal/transport/rest/users/ docs/swagger.yaml docs/swagger.json
git commit -m "docs(swagger): annotate users endpoints with examples"
```

---

## Task 4: Full-spec verification

Final gate: confirm the committed spec covers all 11 endpoints, is valid Swagger 2.0, and the toolchain is clean. Deliverable: a verified, committed spec (this task should require no code changes if Tasks 1–3 were correct).

**Files:**
- Verify only: `docs/swagger.yaml`, `docs/swagger.json`, `go.mod`, `go.sum`.

**Interfaces:**
- Consumes: everything generated in Tasks 1–3.
- Produces: nothing (verification gate).

- [ ] **Step 1: Regenerate from a clean state**

Run: `make swagger`
Expected: `create swagger.yaml at docs/swagger.yaml`; `docs/docs.go` is then removed by the target.

- [ ] **Step 2: Verify all 11 endpoints are present**

Run:
```bash
for p in \
  "/health:" "/health/ready:" "/.well-known/jwks.json:" \
  "/api/v1/auth/signup:" "/api/v1/auth/login:" "/api/v1/auth/otp/request:" \
  "/api/v1/auth/otp/verify:" "/api/v1/auth/refresh:" "/api/v1/auth/logout:" \
  "/api/v1/users/me:"; do
  grep -q "$p" docs/swagger.yaml && echo "have $p" || echo "MISSING $p"
done
```
Expected: ten `have ...` lines and zero `MISSING` (note `/api/v1/users/me` covers both GET and PATCH under one path key — 11 operations across 10 path keys).

- [ ] **Step 3: Verify it is valid Swagger 2.0 JSON**

Run: `python3 -c "import json,sys; d=json.load(open('docs/swagger.json')); assert d['swagger']=='2.0'; print('paths:', len(d['paths']), 'defs:', len(d['definitions']))"`
Expected: prints `paths: 10 defs: N` (N ≥ 7) without an assertion error — confirms the JSON parses and declares Swagger 2.0.

- [ ] **Step 4: Confirm no dependency crept in and docs.go is gone**

Run: `test ! -f docs/docs.go && git diff --stat go.mod go.sum && echo CLEAN`
Expected: no go.mod/go.sum output, prints `CLEAN`.

- [ ] **Step 5: Confirm the spec files are tracked (not caught by .gitignore)**

Run: `git check-ignore docs/swagger.yaml docs/swagger.json; echo "exit=$?"`
Expected: `exit=1` with no path printed (neither file is ignored). If either is printed, add a `!docs/swagger.*` negation to `.gitignore` and re-commit.

- [ ] **Step 6: Final commit (if regeneration changed anything)**

```bash
git add docs/swagger.yaml docs/swagger.json
git commit -m "docs(swagger): finalize full endpoint spec" || echo "nothing to commit"
```

---

## Self-Review notes

- **Spec coverage:** All 11 endpoints mapped to tasks (system→T1, auth→T2, users→T3, full check→T4). Envelope composition, examples, security scheme, no-new-dep constraint, docs.go removal, Swagger 2.0 format — each has explicit steps and verification.
- **Placeholder scan:** No TBD/TODO; every annotation block and struct is spelled out in full.
- **Type consistency:** Definition names (`auth.tokenPairResponse`, `users.userResponse`, `response.Envelope`) match the actual package + type names verified in the repo and in the de-risk run. Security scheme is `BearerAuth` everywhere. `make swagger` command is identical across all tasks.
