#!/usr/bin/env bash
# One-line manual rollback to the previous known-good version. Run on the
# target server (see deploy/README.md for the full one-liner from a laptop).
#
#   ssh -i ~/.ssh/bookeat_deploy ubuntu@<host> '/opt/bookeat/deploy/scripts/rollback.sh'
#
# Swaps IMAGE_TAG <-> PREVIOUS_IMAGE_TAG in .env, restarts app+worker on the
# previous image, and health-checks. Never touches migrations — see
# deploy/README.md if the bad deploy included a non-reversible migration.
set -euo pipefail

DEPLOY_DIR="/opt/bookeat/deploy"
COMPOSE="docker compose --env-file .env"
HEALTH_URL="http://127.0.0.1/health"
HEALTH_RETRIES=10
HEALTH_DELAY=3
HISTORY_FILE="$DEPLOY_DIR/release-history.log"
HISTORY_KEEP=3

cd "$DEPLOY_DIR"

CURRENT_TAG="$(grep -E '^IMAGE_TAG=' .env | cut -d= -f2-)"
PREVIOUS_TAG="$(grep -E '^PREVIOUS_IMAGE_TAG=' .env | cut -d= -f2- || true)"

if [ -z "${PREVIOUS_TAG:-}" ]; then
  echo "!! no PREVIOUS_IMAGE_TAG recorded in .env — nothing to roll back to. Pick a tag manually:"
  echo "   sed -i 's/^IMAGE_TAG=.*/IMAGE_TAG=<tag>/' .env && docker compose pull app worker && docker compose up -d app worker"
  exit 1
fi

echo "== rolling back: current=${CURRENT_TAG} -> previous=${PREVIOUS_TAG} =="

sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=${PREVIOUS_TAG}/" .env
sed -i "s/^PREVIOUS_IMAGE_TAG=.*/PREVIOUS_IMAGE_TAG=${CURRENT_TAG}/" .env

$COMPOSE pull app worker || true
$COMPOSE up -d app worker

n=0
ok=0
while [ "$n" -lt "$HEALTH_RETRIES" ]; do
  if curl -fsS --max-time 3 "$HEALTH_URL" >/dev/null 2>&1; then
    ok=1
    break
  fi
  n=$((n + 1))
  sleep "$HEALTH_DELAY"
done

touch "$HISTORY_FILE"
if [ "$ok" -eq 1 ]; then
  echo "== ROLLED BACK manually: ${PREVIOUS_TAG} is now CURRENT (was ${CURRENT_TAG}) =="
  echo "$(date -Iseconds)	${PREVIOUS_TAG}	manual-rollback	healthy: manual rollback from ${CURRENT_TAG}" >> "$HISTORY_FILE"
else
  echo "!! rollback target ${PREVIOUS_TAG} FAILED health check too — service may be down. Page someone."
  echo "$(date -Iseconds)	${PREVIOUS_TAG}	manual-rollback	CRITICAL: rollback target also failed health check" >> "$HISTORY_FILE"
fi
tail -n "$HISTORY_KEEP" "$HISTORY_FILE" > "${HISTORY_FILE}.tmp" && mv "${HISTORY_FILE}.tmp" "$HISTORY_FILE"
chmod 600 "$HISTORY_FILE"

[ "$ok" -eq 1 ]
