#!/usr/bin/env bash
# Runs ON THE TARGET SERVER (invoked by .github/workflows/deploy.yml over SSH,
# or manually: ssh ubuntu@host 'bash -s' -- <repo> <tag> <actor> < remote-deploy.sh).
#
# Three-version scheme: "previous" (last known-good before this run),
# "current" (what was live when this run started), "new" (what we're
# deploying now). Only after the new image passes its health check does it
# become "current" and the old "current" become "previous". A failed health
# check auto-rolls back to "previous" and says so loudly.
#
# Keeps release-history.log (last 3 attempts: tag, time, who, result) next to
# .env, and prunes local images so only the last 3 distinct versions remain
# on disk. Never touches migrations on rollback (see deploy/README.md — a bad
# migration is not auto-reverted).
set -euo pipefail

IMAGE_REPO="${1:?usage: remote-deploy.sh <image_repo> <image_tag> [actor]}"
NEW_TAG="${2:?usage: remote-deploy.sh <image_repo> <image_tag> [actor]}"
ACTOR="${3:-manual}"
DEPLOY_DIR="/opt/bookeat/deploy"
COMPOSE="docker compose --env-file .env"
# Health check target. NOT http://127.0.0.1/health on the host: that hits Caddy,
# which answers 308 (HTTP->HTTPS redirect) before the request ever reaches the
# app, and `curl -f` treats 3xx as success. The old check therefore passed with
# the application completely dead, which made auto-rollback decorative.
# Fixed 2026-09-02: ask the app itself, inside its own container.
APP_SERVICE="app"
APP_HEALTH_URL="http://127.0.0.1:8080/health"
HEALTH_RETRIES=10
HEALTH_DELAY=3
EDGE_RETRIES=8
EDGE_DELAY=3
HISTORY_FILE="$DEPLOY_DIR/release-history.log"
HISTORY_KEEP=3
IMAGES_KEEP=3

cd "$DEPLOY_DIR"

PREV_TAG="$(grep -E '^IMAGE_TAG=' .env | cut -d= -f2-)"
echo "== current tag: ${PREV_TAG:-<none>}; deploying: ${NEW_TAG} (actor: ${ACTOR}) =="

set_tag() {
  local tag="$1"
  sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=${tag}/" .env
  sed -i "s#^IMAGE_REPO=.*#IMAGE_REPO=${IMAGE_REPO}#" .env
}

set_previous_tag() {
  # Record what "previous" means for the one-line manual rollback
  # (deploy/scripts/rollback.sh) and for README's rollback command.
  local tag="$1"
  if grep -q '^PREVIOUS_IMAGE_TAG=' .env; then
    sed -i "s/^PREVIOUS_IMAGE_TAG=.*/PREVIOUS_IMAGE_TAG=${tag}/" .env
  else
    echo "PREVIOUS_IMAGE_TAG=${tag}" >> .env
  fi
}

record_history() {
  # timestamp | tag | actor | result
  local result="$1"
  touch "$HISTORY_FILE"
  echo "$(date -Iseconds)	${NEW_TAG}	${ACTOR}	${result}" >> "$HISTORY_FILE"
  # Keep only the last N entries — full audit trail lives in GitHub Actions
  # run logs; this file is a quick on-box "what happened last" reference.
  tail -n "$HISTORY_KEEP" "$HISTORY_FILE" > "${HISTORY_FILE}.tmp" && mv "${HISTORY_FILE}.tmp" "$HISTORY_FILE"
  chmod 600 "$HISTORY_FILE"
}

prune_old_images() {
  # Dedup by image ID (current/previous/branch tags may share a digest with
  # one of the SHA tags), keep the IMAGES_KEEP most recently created, drop
  # the rest so old versions don't accumulate on disk.
  docker images --format '{{.ID}}|{{.Repository}}|{{.CreatedAt}}' \
    | awk -F'|' -v repo="$IMAGE_REPO" '$2==repo' \
    | sort -t'|' -k3 -r \
    | awk -F'|' '!seen[$1]++ {print $1}' \
    | tail -n +"$((IMAGES_KEEP + 1))" \
    | while IFS= read -r id; do
        [ -n "$id" ] || continue
        echo "== pruning old image: ${id} =="
        docker rmi -f "$id" >/dev/null 2>&1 || echo "!! could not remove ${id} (maybe still in use), skipping"
      done
}

app_health_once() {
  # busybox wget: non-zero exit on connection failure AND on any non-2xx
  # HTTP status. Runs inside the app container, so it depends on neither
  # Caddy, nor TLS, nor external DNS — it can only succeed if the process we
  # just deployed is actually serving.
  local body
  body="$($COMPOSE exec -T "$APP_SERVICE" wget -q -T 3 -O - "$APP_HEALTH_URL" 2>/dev/null)" || return 1
  [ -n "$body" ] || return 1
  return 0
}

check_health() {
  local n=0
  while [ "$n" -lt "$HEALTH_RETRIES" ]; do
    if app_health_once; then
      return 0
    fi
    n=$((n + 1))
    sleep "$HEALTH_DELAY"
  done
  return 1
}

check_edge() {
  # Informational only: does the same /health answer through Caddy on this
  # box? Pinned to 127.0.0.1 with the real hostname so TLS/SNI works without
  # depending on public DNS. Deliberately NOT a rollback trigger: a deploy
  # only swaps app+worker images, so if the app is healthy but the edge is
  # not, rolling the app back would not fix Caddy/TLS — it would just hide it.
  local site code
  site="$(grep -E '^SITE_ADDRESS=' .env | cut -d= -f2- || true)"
  site="${site#http://}"
  site="${site#https://}"
  site="${site%%/*}"
  case "$site" in
    ""|:*|*[!a-zA-Z0-9.-]*) echo "-- edge check skipped: SITE_ADDRESS is not a hostname"; return 0 ;;
  esac
  # Caddy needs a few seconds to re-resolve the freshly recreated app
  # container (observed 503 for ~4s on the test box on 2026-09-02), so retry
  # before complaining.
  local n=0
  code=000
  while [ "$n" -lt "$EDGE_RETRIES" ]; do
    code="$(curl -sS -o /dev/null --max-time 5 --resolve "${site}:443:127.0.0.1" -w '%{http_code}' "https://${site}/health" 2>/dev/null || echo 000)"
    [ "$code" = "200" ] && break
    n=$((n + 1))
    sleep "$EDGE_DELAY"
  done
  if [ "$code" = "200" ]; then
    echo "-- edge check OK: https://${site}/health -> 200"
  else
    echo "!! WARNING: app is healthy inside the container, but the edge"
    echo "!! https://${site}/health answered ${code}. Not rolling back (the app"
    echo "!! image is fine) — check Caddy / certificates / firewall."
  fi
}

echo "== pulling ${IMAGE_REPO}:${NEW_TAG} =="
set_tag "$NEW_TAG"
$COMPOSE pull app worker migrate

echo "== running migrations (before restart) =="
$COMPOSE run --rm migrate

echo "== restarting app + worker with the NEW version =="
$COMPOSE up -d app worker

echo "== health check =="
if check_health; then
  echo "== deploy OK: ${NEW_TAG} is now CURRENT (was: ${PREV_TAG:-<none>}, now PREVIOUS) =="
  if [ -n "${PREV_TAG:-}" ]; then
    set_previous_tag "$PREV_TAG"
  fi
  record_history "healthy: promoted to current, previous was ${PREV_TAG:-<none>}"
  check_edge
  prune_old_images
  exit 0
fi

echo "== HEALTH CHECK FAILED — rolling back to ${PREV_TAG:-<none>} (that stays CURRENT) =="
if [ -z "${PREV_TAG:-}" ]; then
  echo "!! no previous tag recorded, cannot auto-rollback. Manual intervention required."
  record_history "FAILED: no previous tag to roll back to, service may be degraded"
  exit 1
fi

set_tag "$PREV_TAG"
$COMPOSE pull app worker || true
$COMPOSE up -d app worker

if check_health; then
  echo "== ROLLED BACK to ${PREV_TAG}: this is now the confirmed CURRENT version. ${NEW_TAG} was NOT promoted. =="
  record_history "ROLLED BACK: ${NEW_TAG} failed health check, reverted to ${PREV_TAG}"
else
  echo "!! rollback ALSO failed health check — service may be down. Page someone."
  record_history "CRITICAL: ${NEW_TAG} failed health check AND rollback to ${PREV_TAG} also failed"
fi
exit 1
