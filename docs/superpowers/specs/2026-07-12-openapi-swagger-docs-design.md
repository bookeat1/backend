# OpenAPI / Swagger documentation for all endpoints — design

Date: 2026-07-12
Status: Approved (pending spec review)

## Goal

Document **every** HTTP endpoint of `backend-core` as a Swagger 2.0 spec (the
format swaggo v1 emits; fully supported by editor.swagger.io and Postman), with
clear per-endpoint descriptions and concrete request/response **examples**. The
artifact is a committed spec file openable in editor.swagger.io / Postman — the
service does **not** serve it.

## Approach

Use **swaggo/swag** declarative comment annotations on the Gin handlers. Generate
the spec with `swag init`, then commit **only** `docs/swagger.yaml` and
`docs/swagger.json`. Delete the generated `docs/docs.go` so **no new dependency**
lands in `go.mod` (CLAUDE.md: minimal deps). The generator runs via
`go run github.com/swaggo/swag/cmd/swag@latest ...`, never as a project import.

Decisions locked in brainstorming:
- Format: swaggo annotations (docs live next to code).
- Delivery: file-only, no serving route, no `gin-swagger`.
- `docs.go`: removed after generation → zero go.mod deps.
- Health/JWKS: **do not touch `app.go`**; document via a stub annotation file.

## Endpoint inventory (all must appear in the spec)

Unauthenticated:
- `GET /health` — liveness. 200 `{ "data": { "status": "ok" } }`.
- `GET /health/ready` — readiness (pings DB). 200 `{ "data": { "status": "ready" } }`;
  503 `{ "data": { "status": "unavailable" } }`.
- `GET /.well-known/jwks.json` — public JWKS for RS256 verification. 200
  `{ "keys": [ { "kty": "RSA", "use": "sig", "alg": "RS256", "kid": "...", "n": "...", "e": "..." } ] }`.
  (Raw JWKS, **not** wrapped in the envelope.)

Auth (unauthenticated), prefix `/api/v1/auth`:
- `POST /signup` — email+password+full_name → **201** token pair.
- `POST /login` — email+password → 200 token pair.
- `POST /otp/request` — phone → 200 `{ "data": { "sent": true, "code": "123456" } }`
  (`code` present only when `AUTH_OTP_DEV_EXPOSE=true`).
- `POST /otp/verify` — phone+code → 200 token pair.
- `POST /refresh` — refresh_token → 200 token pair.
- `POST /logout` — refresh_token → 200 `{ "data": { "ok": true } }`.

Authenticated (`Authorization: Bearer <access_token>`), prefix `/api/v1/users`:
- `GET /me` — current user profile → 200 user object.
- `PATCH /me` — partial profile update → 200 user object.

### Shared shapes

- **Envelope**: every handler response (except JWKS) is
  `{ "data": <payload> }` on success or `{ "error": "<message>" }` on failure
  (`response.Envelope`). Model in swaggo via composition:
  `response.Envelope{data=auth.tokenPairResponse}`.
- **Token pair** (`tokenPairResponse`): `access_token` (string, JWT),
  `refresh_token` (string, opaque), `expires_at` (RFC3339, access-token expiry).
- **User** (`userResponse`): `id` (uuid), `email` (string|null), `phone`
  (string|null), `full_name`, `role` (`user`|`restaurant`|`admin`), `avatar_url`
  (string|null), `preferred_language`, `city` (string|null), `email_verified_at`
  (RFC3339|null), `phone_verified_at` (RFC3339|null), `created_at` (RFC3339).
- **Error responses**: generic messages from `response.HandleError`:
  404 `not found`, 409 `already exists`, 403 `forbidden`, 401 `unauthorized`,
  422 `validation failed` / `invalid status transition`, 500 `internal server error`.
  Binding failures return 422 with the validator's raw message.

## Components / changes

1. **General API info + security scheme.** A file `cmd/http/swagger.go` (or the
   doc comment above `main()`) carrying `// @title BookEat backend-core API`,
   `// @version 1.0`, `// @description ...`, and
   `// @securityDefinitions.apikey BearerAuth` / `// @in header` /
   `// @name Authorization`. No global `@BasePath` (endpoints span `/` and
   `/api/v1`); each handler declares its full path in `@Router`.

2. **Auth handler annotations** (`internal/transport/rest/auth/handler.go`):
   per method `@Summary`, `@Description`, `@Tags auth`, `@Accept json`,
   `@Produce json`, `@Param body body <reqDTO> true "..."`, `@Success` /
   `@Failure` with `response.Envelope{data=...}` composition, `@Router`.

3. **Users handler annotations** (`internal/transport/rest/users/handler.go`):
   as above, `@Tags users`, plus `@Security BearerAuth`. `PATCH /me` documents the
   partial-update body (all fields optional/nullable).

4. **Example values** via `example:"..."` struct tags on DTO fields in
   `auth/request.go`, `auth/response.go`, `users/request.go`, `users/response.go`.
   This is the single source for request/response examples — no hand-duplicated
   JSON. Confirm swaggo resolves the **unexported** DTO types (annotations live in
   the same package as the types, so AST resolution works); if any type fails to
   resolve, the fallback is to export it or add an explicit example on the field.

5. **Health + JWKS annotations without touching `app.go`.** A stub file
   `cmd/http/swagger.go` (same file as general info) holds dummy, never-called
   functions carrying only the swaggo comment blocks for `GET /health`,
   `GET /health/ready`, and `GET /.well-known/jwks.json` (`@Tags system`). Go
   permits unused package-level functions, so this compiles cleanly and keeps
   `app.go` untouched.

6. **Generation wiring.**
   - `Makefile` target `swagger`:
     `go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/http/swagger.go --parseInternal -o docs && rm -f docs/docs.go`
     (flags finalized during implementation; `--parseInternal` needed because
     handlers live under `internal/`).
   - Commit `docs/swagger.yaml`, `docs/swagger.json`.
   - `docs/docs.go` is generated then removed; ensure it is not committed and not
     imported anywhere.

7. **Verification.** `go build ./...` and `go vet ./...` pass (stub funcs + tags
   compile). `swag init` completes without warnings and the generated spec is
   valid OpenAPI (validate by loading `docs/swagger.yaml` in a validator).
   Confirm all 11 endpoints and both shared schemas (`tokenPairResponse`,
   `userResponse`) appear with examples.

## Out of scope

- Serving the spec / Swagger UI from the service.
- `gin-swagger`, `swaggo/files`, or any runtime swagger dependency in `go.mod`.
- CI step to check the committed spec is up to date (can be a follow-up).
- Documenting request/response for future endpoints not yet implemented.

## Risks / notes

- **Unexported DTOs**: main uncertainty. Mitigation in component 4.
- **`swag init` path flags**: `-g` + `--parseInternal` (and possibly `-d`) must be
  tuned so swag discovers annotations under `internal/`; validated during impl.
- Spec can drift from code (inherent to annotations-without-CI); accepted, CI
  guard is a noted follow-up.
