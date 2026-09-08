# syntax=docker/dockerfile:1

# ---- builder ------------------------------------------------------------
# Pinned to the exact toolchain version in go.mod (go 1.25.7) so a CI build
# and a local build produce the same binary. Bump both together.
FROM golang:1.25.7-alpine3.22 AS builder

# build-base (gcc + musl-dev + friends) and libwebp-dev are here for exactly
# one reason: internal/media encodes derivatives with
# github.com/kolesa-team/go-webp, a cgo binding over libwebp. There is no
# pure-Go WebP encoder of acceptable quality, and the product owner decided
# (2026-09-06) that photo quality/size matters more than keeping this a
# CGO_ENABLED=0 build — see internal/media/key.go. That is also why
# CGO_ENABLED is 1 below, not 0 as it used to be.
RUN apk add --no-cache git ca-certificates build-base libwebp-dev

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64

# Three binaries out of one module: the HTTP API, the background booking
# worker, and the goose-based migrator. Same image, different ENTRYPOINT
# override per compose service — one build, one thing to keep in sync.
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/http    ./cmd/http \
 && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/worker  ./cmd/worker \
 && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/migrate ./cmd/migrate

# ---- final ----------------------------------------------------------------
# Alpine, not distroless: the HEALTHCHECK below needs `wget` and there is no
# shell-free equivalent worth shipping a custom binary for. Still a ~15MB
# final image, non-root user, no build toolchain.
FROM alpine:3.22

# libwebp (the runtime shared library, not -dev) is required at startup: the
# http/worker binaries were dynamically linked against it once CGO_ENABLED=1
# turned on for the WebP encoder in internal/media (see the builder stage
# comment). Without it the binary fails to even start, not just to resize.
RUN apk add --no-cache ca-certificates tzdata wget libwebp \
 && addgroup -S -g 10001 app \
 && adduser  -S -u 10001 -G app -h /app -s /sbin/nologin app

WORKDIR /app
COPY --from=builder /out/http /out/worker /out/migrate ./

USER app:app

ENV APP_URL=0.0.0.0:8080
EXPOSE 8080

# Liveness: the process answers on /health without touching the DB. Readiness
# (DB reachable) is a separate endpoint (/health/ready), deliberately not used
# here — a slow DB should not make Docker kill and restart a healthy process.
#
# NOTE: `wget --spider` sends HEAD, and gin's r.GET() does not auto-answer
# HEAD (unlike some frameworks) — a spider check 404s even though the route
# works. Use a real GET, discarding the body.
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/http"]
