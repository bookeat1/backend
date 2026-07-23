#!/usr/bin/env bash
# BookEat: nightly Postgres backup (custom format, pg_dump -Fc) with rotation,
# integrity check, and an off-box copy to Cloudflare R2 (S3-compatible) via
# rclone. Runs on the same box as the DB.
#
# Retention (local disk): 7 daily + 4 weekly (weekly = Sunday's daily dump,
# hardlinked into the weekly dir so we don't dump twice).
# Retention (cloud, R2): 30 days daily / 90 days weekly, enforced by R2
# bucket lifecycle rules (prefix-scoped), not by this script — see
# deploy/README.md.
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

# --- Cloud (R2) upload config ---
# ENVIRONMENT_FILE contains exactly "prod" or "test" — set once per server at
# install time, independent of app-level env naming (APP_ENV=production/staging).
ENVIRONMENT_FILE="$BASE_DIR/environment"
RCLONE_CONFIG_FILE="$BASE_DIR/rclone.conf"
RCLONE_BIN="${RCLONE_BIN:-/usr/local/bin/rclone}"
R2_REMOTE="r2"
R2_BUCKET="book-eat"

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

# --- Cloud upload (off-box copy, mandatory) ---
# Uploads to <environment>/daily/<file> and, on Sundays, <environment>/weekly/<file>
# in the R2 bucket. A failed or unverifiable upload fails the whole backup run
# (non-zero exit) — a local-only dump is not considered "done" for this box.
# Cloud-side retention (30d daily / 90d weekly) is enforced by bucket lifecycle
# rules, not by this script (see deploy/README.md) — no cloud pruning here.
[ -f "$ENVIRONMENT_FILE" ] || fail "environment marker not found: $ENVIRONMENT_FILE (expected file containing 'prod' or 'test')"
ENVIRONMENT="$(tr -d '[:space:]' < "$ENVIRONMENT_FILE")"
case "$ENVIRONMENT" in
  prod|test) ;;
  *) fail "invalid environment marker '$ENVIRONMENT' in $ENVIRONMENT_FILE (expected 'prod' or 'test')" ;;
esac
[ -f "$RCLONE_CONFIG_FILE" ] || fail "CLOUD_UPLOAD_FAILED: rclone config not found: $RCLONE_CONFIG_FILE"
[ -x "$RCLONE_BIN" ] || fail "CLOUD_UPLOAD_FAILED: rclone binary not found/executable: $RCLONE_BIN"

upload_to_cloud() {
  # $1 = local file, $2 = "daily" or "weekly"
  local local_file="$1" kind="$2"
  local base_name local_dir remote_dir remote_path local_size remote_bytes

  base_name="$(basename "$local_file")"
  local_dir="$(dirname "$local_file")"
  remote_dir="${R2_REMOTE}:${R2_BUCKET}/${ENVIRONMENT}/${kind}"
  remote_path="${remote_dir}/${base_name}"
  local_size="$(stat -c%s "$local_file")"

  log "cloud upload starting: $local_file ($local_size bytes) -> $remote_path"
  if ! RCLONE_CONFIG="$RCLONE_CONFIG_FILE" "$RCLONE_BIN" copyto \
      "$local_file" "$remote_path" --s3-no-check-bucket >>"$LOG_FILE" 2>&1; then
    fail "CLOUD_UPLOAD_FAILED: rclone copyto exited non-zero for $local_file -> $remote_path"
  fi

  # Verify #1: size, straight from the object metadata R2 just returned.
  remote_bytes="$(RCLONE_CONFIG="$RCLONE_CONFIG_FILE" "$RCLONE_BIN" size --json "$remote_path" 2>>"$LOG_FILE" \
    | grep -oE '"bytes":[0-9]+' | cut -d: -f2)"
  if [ -z "$remote_bytes" ] || [ "$remote_bytes" != "$local_size" ]; then
    fail "CLOUD_UPLOAD_VERIFY_FAILED: size mismatch for $remote_path (local=$local_size remote=${remote_bytes:-unknown})"
  fi
  log "cloud upload size OK: $remote_path ($remote_bytes bytes, matches local)"

  # Verify #2: checksum, via rclone check (MD5, the hash R2 actually supports)
  # scoped to this one file with --include so it doesn't compare the whole
  # daily/weekly prefix.
  if ! RCLONE_CONFIG="$RCLONE_CONFIG_FILE" "$RCLONE_BIN" check "$local_dir" "$remote_dir" \
      --include "$base_name" --one-way >>"$LOG_FILE" 2>&1; then
    fail "CLOUD_UPLOAD_VERIFY_FAILED: checksum mismatch between $local_file and $remote_path"
  fi
  log "cloud upload checksum OK: $remote_path matches $local_file — verified upload"
}

upload_to_cloud "$DUMP_FILE" "daily"
if [ "$DOW" = "7" ]; then
  upload_to_cloud "$WEEKLY_FILE" "weekly"
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
