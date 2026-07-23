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
HEALTH_URL="http://127.0.0.1/health"
HEALTH_RETRIES=10
HEALTH_DELAY=3
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

check_health() {
  local n=0
  while [ "$n" -lt "$HEALTH_RETRIES" ]; do
    if curl -fsS --max-time 3 "$HEALTH_URL" >/dev/null 2>&1; then
      return 0
    fi
    n=$((n + 1))
    sleep "$HEALTH_DELAY"
  done
  return 1
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
