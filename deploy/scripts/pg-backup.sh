#!/usr/bin/env bash
# BookEat: nightly Postgres backup (custom format, pg_dump -Fc) with rotation
# and integrity check. Runs on the same box as the DB (see deploy runbook for
# the "off-box copy" follow-up — not implemented here).
#
# Retention: 7 daily + 4 weekly (weekly = Sunday's daily dump, hardlinked into
# the weekly dir so we don't dump twice).
#
# Installed at /opt/bookeat/backups/pg-backup.sh, run by cron as the ubuntu
# user (docker group member) at 02:30 Asia/Almaty.
set -euo pipefail

DEPLOY_DIR="/opt/bookeat/deploy"
BASE_DIR="/opt/bookeat/backups"
DAILY_DIR="$BASE_DIR/daily"
WEEKLY_DIR="$BASE_DIR/weekly"
LOG_FILE="$BASE_DIR/backup.log"
DAILY_KEEP=7
WEEKLY_KEEP=4

ts() { date '+%Y-%m-%d %H:%M:%S %z'; }
log() { echo "[$(ts)] $*" | tee -a "$LOG_FILE" >&2; }

fail() {
  log "FAIL: $*"
  exit 1
}

trap 'log "FAIL: unexpected error at line $LINENO"' ERR

[ -f "$DEPLOY_DIR/.env" ] || fail ".env not found at $DEPLOY_DIR/.env"

# shellcheck disable=SC1091
DB_USERNAME="$(grep -E '^DB_USERNAME=' "$DEPLOY_DIR/.env" | cut -d= -f2-)"
DB_DATABASE="$(grep -E '^DB_DATABASE=' "$DEPLOY_DIR/.env" | cut -d= -f2-)"
DB_USERNAME="${DB_USERNAME:-bookeat}"
DB_DATABASE="${DB_DATABASE:-bookeat}"

mkdir -p "$DAILY_DIR" "$WEEKLY_DIR"
chmod 700 "$BASE_DIR" "$DAILY_DIR" "$WEEKLY_DIR"

STAMP="$(date +%Y%m%d-%H%M%S)"
DOW="$(date +%u)"   # 1=Mon .. 7=Sun
DUMP_FILE="$DAILY_DIR/bookeat-${STAMP}.dump"

log "starting dump: db=$DB_DATABASE user=$DB_USERNAME -> $DUMP_FILE"

cd "$DEPLOY_DIR"
umask 077
if ! docker compose exec -T postgres pg_dump -U "$DB_USERNAME" -d "$DB_DATABASE" -Fc > "$DUMP_FILE"; then
  rm -f "$DUMP_FILE"
  fail "pg_dump failed"
fi
chmod 600 "$DUMP_FILE"

[ -s "$DUMP_FILE" ] || fail "dump file is empty: $DUMP_FILE"

log "verifying dump integrity with pg_restore --list"
# No postgresql-client on the host on purpose (least install footprint) — use
# the same postgres:16.6-alpine image already pulled for the DB container,
# read-only mount, ephemeral container, no network.
PG_IMAGE="$(grep -oE 'image: postgres:[^[:space:]]+' "$DEPLOY_DIR/docker-compose.yml" | head -1 | cut -d' ' -f2)"
PG_IMAGE="${PG_IMAGE:-postgres:16.6-alpine}"
LISTING_FILE="/tmp/pg-backup-listing.$$"
if ! docker run --rm --network none \
    -v "$DAILY_DIR":/backups:ro \
    "$PG_IMAGE" pg_restore --list "/backups/$(basename "$DUMP_FILE")" > "$LISTING_FILE" 2>>"$LOG_FILE"; then
  rm -f "$LISTING_FILE"
  fail "pg_restore --list could not read $DUMP_FILE — dump is corrupt"
fi
ENTRY_COUNT="$(wc -l < "$LISTING_FILE")"
rm -f "$LISTING_FILE"
[ "$ENTRY_COUNT" -gt 0 ] || fail "dump listing is empty — dump is corrupt"
log "OK: dump has $ENTRY_COUNT catalog entries"

# Weekly copy: Sunday's daily dump is hardlinked into weekly/.
if [ "$DOW" = "7" ]; then
  WEEKLY_FILE="$WEEKLY_DIR/bookeat-${STAMP}.dump"
  ln "$DUMP_FILE" "$WEEKLY_FILE"
  chmod 600 "$WEEKLY_FILE"
  log "weekly copy created: $WEEKLY_FILE"
fi

# Rotation: keep last N daily, last M weekly.
prune_dir() {
  local dir="$1" keep="$2"
  local count
  count="$(find "$dir" -maxdepth 1 -name 'bookeat-*.dump' | wc -l)"
  if [ "$count" -gt "$keep" ]; then
    find "$dir" -maxdepth 1 -name 'bookeat-*.dump' -printf '%T@ %p\n' \
      | sort -n | head -n "$((count - keep))" | cut -d' ' -f2- \
      | while IFS= read -r f; do
          log "pruning old backup: $f"
          rm -f "$f"
        done
  fi
}
prune_dir "$DAILY_DIR" "$DAILY_KEEP"
prune_dir "$WEEKLY_DIR" "$WEEKLY_KEEP"

log "backup finished OK: $DUMP_FILE ($(du -h "$DUMP_FILE" | cut -f1))"
exit 0
