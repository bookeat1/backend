# Deploy runbook — bookeat-backend

One stack per server (`/opt/bookeat` on both `prod` and `test`): Postgres 16,
the Go app, the background worker, Caddy. Compose file: `deploy/docker-compose.yml`.
Config: `deploy/.env` (not committed, `chmod 600 ubuntu:ubuntu`).

## Layout on the server

```
/opt/bookeat/
├── docker-compose.yml
├── Caddyfile
└── .env            # chmod 600, real secrets, never in git
```

The application source/Dockerfile do not need to live on the server once CI
publishes images to GHCR — `docker compose pull` fetches `IMAGE_REPO:IMAGE_TAG`.
For the first manual bring-up (no CI yet) the whole repo was rsynced to the
server and images were built locally with `IMAGE_TAG=local`.

## First-time setup (already done by this task, kept here for a future box)

1. Provision the box (see "Server hardening" below: Docker, ufw, fail2ban,
   unattended-upgrades, timezone, swap on small boxes).
2. `mkdir -p /opt/bookeat && chown ubuntu:ubuntu /opt/bookeat`
3. Copy `deploy/docker-compose.yml` and `deploy/Caddyfile` into `/opt/bookeat/`.
4. `cp deploy/.env.example /opt/bookeat/.env`, fill in every blank
   (`DB_PASSWORD`, `AUTH_JWT_PRIVATE_KEY`, PG tuning for this box's RAM), then
   `chmod 600 /opt/bookeat/.env`.
5. `cd /opt/bookeat && docker compose build app worker` (first time only, or
   whenever `IMAGE_TAG=local`).
6. Start Postgres, wait for its healthcheck, run migrations, then start the
   rest — see "Deploy" below.
7. Install nightly backups:
   ```bash
   mkdir -p /opt/bookeat/backups && chmod 700 /opt/bookeat/backups
   cp deploy/scripts/pg-backup.sh /opt/bookeat/backups/pg-backup.sh
   chmod 700 /opt/bookeat/backups/pg-backup.sh
   ( crontab -l 2>/dev/null; echo "30 2 * * * /opt/bookeat/backups/pg-backup.sh >/dev/null 2>&1" ) | crontab -
   ```
   (server timezone must be `Asia/Almaty` — `timedatectl set-timezone Asia/Almaty`
   — so 02:30 in the crontab means 02:30 Almaty time.) See "Backups" below.
8. Enable the off-box (cloud) copy: install `rclone` (pinned version, not the
   distro package — see "Backups" below for why), write
   `/opt/bookeat/backups/environment` containing exactly `prod` or `test`,
   and `/opt/bookeat/backups/rclone.conf` (chmod 600) pointing at the shared
   `r2` bucket. Credentials come from `/home/tai/.bookeat/r2.env` (off-server,
   ask whoever holds it) — never commit them.

## Deploy (manual, from a checked-out repo or CI runner)

```bash
cd /opt/bookeat

# 1. Get the new code/image.
#    Manual/local build:
docker compose build app worker
#    CI / GHCR path (after login, see deploy.yml):
#    docker compose pull app worker migrate

# 2. Bring Postgres up first and wait for it to be healthy.
docker compose up -d postgres
docker compose ps postgres   # STATUS must show "healthy"

# 3. Run migrations BEFORE restarting the app (architecture requirement).
docker compose run --rm migrate   # entrypoint is /app/migrate, command "up"

# 4. (Re)start the app, worker, and proxy.
docker compose up -d app worker caddy

# 5. Verify.
curl -fsS http://127.0.0.1:8080/health          # from inside the box (bypasses Caddy)
curl -fsS http://<server-ip>/health             # through Caddy, from outside
docker compose ps
docker compose logs --tail=100 app
```

`docker compose run --rm migrate` always targets the tag currently in
`IMAGE_TAG` in `.env` — bump that (or re-pull) before running it so migrate
and app are the same build.

## Versioning: previous / current / new

Every image is tagged twice in GHCR:

- an **immutable tag** — the 7-char commit SHA (e.g. `a1b2c3d`). This is the
  only tag that ever determines what actually runs anywhere; it never moves.
- two **moving convenience tags**, `current` and `previous`, updated only
  after a deploy job (`.github/workflows/deploy.yml`) confirms the new image
  passed its health check on a server. These are for humans browsing GHCR
  ("what's the latest healthy build look like") — they are **not** per
  environment, since test and prod deploy independently and at different
  cadences. The actual source of truth for what a given server is running is
  that server's own `deploy/.env` (`IMAGE_TAG`) plus its
  `deploy/release-history.log`.

On each server, `.env` holds:

- `IMAGE_TAG` — the SHA that is CURRENT on *this* server right now.
- `PREVIOUS_IMAGE_TAG` — the SHA that was CURRENT right before that, kept for
  the one-line manual rollback below.

`deploy/release-history.log` (chmod 600, next to `.env`) keeps the last 3
deploy attempts on that server: tab-separated `timestamp / tag / actor /
result`. It's a quick "what just happened" reference; the full log of every
run lives in the GitHub Actions history.

### What happens on a deploy

1. `deploy/scripts/remote-deploy.sh` pulls the new image, runs migrations,
   restarts `app`+`worker` on it, then health-checks through Caddy.
2. **Health check passes** → the new tag is written to `IMAGE_TAG`, the tag
   that was current a moment ago is written to `PREVIOUS_IMAGE_TAG`, the
   attempt is logged as `healthy`, old local images beyond the last 3 are
   pruned (`docker rmi`) so disk doesn't fill up, and (back on the CI runner)
   GHCR's `current`/`previous` tags are moved to match.
2. **Health check fails** → `IMAGE_TAG` is set back to the previous tag,
   `app`/`worker` restart on it, health-checked again, and the run **fails
   loudly** (`ROLLED BACK to <tag>: this is now the confirmed CURRENT
   version. <new-tag> was NOT promoted.` in the logs) so nobody mistakes a
   rollback for a successful deploy. `PREVIOUS_IMAGE_TAG` is left untouched —
   the failed candidate never became current, so "previous" still means what
   it meant before this attempt.
3. Migrations are **never** auto-reverted on rollback (see below) — they ran
   once, before the app restarted, on purpose.

### Manual rollback (one line)

```bash
ssh -i ~/.ssh/bookeat_deploy ubuntu@<server-ip> '/opt/bookeat/deploy/scripts/rollback.sh'
```

Swaps `IMAGE_TAG` and `PREVIOUS_IMAGE_TAG` in `.env`, restarts `app`+`worker`
on the previous image, health-checks, and logs the attempt. If there is no
`PREVIOUS_IMAGE_TAG` recorded (e.g. right after a fresh box's first deploy),
it says so and prints the manual `sed` + restart fallback instead of guessing.

If the bad deploy included a migration that needs reverting too (rare — only
for a migration marked reversible in its own file):

```bash
docker compose run --rm --entrypoint /app/migrate migrate down
```

**A migration without a safe `down` is not rolled back automatically** — stop,
assess, and get explicit sign-off before running anything destructive against
prod data. The GitHub Actions `deploy.yml` workflow automates the image
rollback path (previous tag + health-check) but never touches migrations.

## Logs

```bash
docker compose logs -f app            # follow app logs (json-file driver, capped: see compose file)
docker compose logs -f worker
docker compose logs -f postgres
docker compose logs -f caddy
journalctl -u docker --since "1 hour ago"   # daemon-level issues
```

Container logs are `json-file`, capped at `max-size 20m, max-file 5` per
service (docker-compose.yml `logging:` blocks) — bounded disk use, no external
log shipper yet.

## Connect to the database

```bash
cd /opt/bookeat
docker compose exec postgres psql -U "$DB_USERNAME" -d "$DB_DATABASE"
# or, from outside the container, using the same creds (Postgres is not
# published to the host network — this is the only way in):
docker compose exec postgres pg_isready
```

There is no port published for Postgres to the host or the internet — by
design (least privilege). To connect from a laptop, tunnel over SSH:

```bash
ssh -i ~/.ssh/bookeat_deploy -L 5433:localhost:5433 ubuntu@<server-ip> \
  'docker compose -f /opt/bookeat/docker-compose.yml exec -T postgres psql -U $DB_USERNAME -d $DB_DATABASE'
```

or `docker compose port` a temporary published port only when needed, and
close it again.

## DNS cutover (when the domain is delegated)

The only two changes needed, per server, once DNS resolves to it:

1. `/opt/bookeat/.env`: set `SITE_ADDRESS` to the real hostname
   (`backend.book-eat.com` on prod, `test.backend.book-eat.com` on test).
2. `docker compose up -d caddy` (or `docker compose restart caddy`).

Caddy will then request a Let's Encrypt certificate automatically and start
serving HTTPS on :443 (with :80 redirecting to it). No other file changes —
same `Caddyfile`, same compose file.

## Backups (Postgres)

Automated on both servers as of 2026-07-23:

- `/opt/bookeat/backups/pg-backup.sh` runs nightly via cron at 02:30
  Asia/Almaty (`crontab -l` as `ubuntu`), taking a `pg_dump -Fc` (custom
  format, compressed) of the live database.
- Every dump is verified immediately with `pg_restore --list` (run in a
  throwaway `postgres:16.6-alpine` container, `--network none`, read-only
  mount — no extra client package installed on the host). A dump that fails
  this check is treated as a failed backup: the script exits non-zero and
  logs `FAIL`.
- Retention: 7 daily dumps (`backups/daily/`) + 4 weekly (`backups/weekly/`,
  Sunday's daily dump hardlinked in), older files pruned automatically.
- Permissions: `/opt/bookeat/backups` and its subdirs are `chmod 700`
  (owner `ubuntu` only), dump files `chmod 600`.
- Logs: `/opt/bookeat/backups/backup.log`, one line per step, timestamped.

Restore (tested manually on 2026-07-23 on both servers — see the deploy
report for the exact commands and row-count diff, which matched exactly):

```bash
cd /opt/bookeat/deploy
docker compose cp /opt/bookeat/backups/daily/<file>.dump postgres:/tmp/restore.dump
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "CREATE DATABASE bookeat_restore_test OWNER $DB_USERNAME;"
docker compose exec -T postgres pg_restore -U "$DB_USERNAME" \
  -d bookeat_restore_test --no-owner /tmp/restore.dump
# ... verify tables/row counts, then:
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "DROP DATABASE bookeat_restore_test;"
```

**Off-box copy (2026-07-23): done.** Every dump (daily always, weekly on
Sundays) is also uploaded to Cloudflare R2 (S3-compatible object storage,
bucket `book-eat`) right after the local integrity check, via `rclone`. This
closes the gap above — a lost/corrupted/ransomed server no longer means a
lost backup.

- Layout in the bucket: `<environment>/daily/<file>.dump` and
  `<environment>/weekly/<file>.dump`, where `<environment>` is `prod` or
  `test` (read from `/opt/bookeat/backups/environment` on each server — a
  one-word marker file, independent of `APP_ENV`).
- `rclone` binary: `/usr/local/bin/rclone` **v1.68.2**, installed from the
  official `downloads.rclone.org` zip, not the Ubuntu 24.04 apt package
  (`1.60.1`, from 2022) — the apt version returns a `501 Not Implemented` on
  the first upload attempt against R2 (silently retries and succeeds, but
  it's noise worth avoiding; the modern binary doesn't do this). Same exact
  version pinned on both servers.
- `rclone` config: `/opt/bookeat/backups/rclone.conf` (chmod 600, remote name
  `r2`, S3 provider `Cloudflare`). Credentials also kept as plain env vars at
  `/opt/bookeat/backups/r2.env` (chmod 600) for reference/rotation — not read
  by the backup script itself, only used to (re)generate `rclone.conf`.
- **Upload is verified, not assumed**: after `rclone copyto`, the script
  compares (a) size via `rclone size --json` against the local file's
  `stat -c%s`, and (b) checksum via `rclone check --include <file> --one-way`
  (MD5 — the hash R2's S3 API actually exposes). Either mismatch, or the
  upload command itself failing, makes the whole backup run exit non-zero
  and log a line starting `FAIL: CLOUD_UPLOAD_FAILED` or
  `FAIL: CLOUD_UPLOAD_VERIFY_FAILED` — grep either string in
  `/opt/bookeat/backups/backup.log` (or cron's mail/journal output) to alert
  on it. A local-only dump is **not** treated as a completed backup anymore.
- Cloud retention: 30 days for `*/daily/*`, 90 days for `*/weekly/*`, enforced
  by R2 **bucket lifecycle rules** (age-based, prefix-scoped) set via the
  Cloudflare API — not by the script. The script never deletes anything in
  the bucket. Local disk retention (7 daily / 4 weekly) is unchanged.
- Nothing here changes the local backup path or its 02:30 Asia/Almaty
  schedule — the cloud upload is an additional step in the same script, same
  cron entry.

### Restoring from the cloud copy (3am-with-shaking-hands version)

Use this when the server that made the backup is gone, or you specifically
need to prove the cloud copy (not just the local disk copy) is good. If the
box and its local `backups/daily/` still exist, the faster path is the local
restore steps above — this section is for when they don't.

You need: SSH access to *any* box with `rclone` configured against the `r2`
remote (either bookeat server, or your laptop with a copy of
`rclone.conf`), and SSH/docker access to whatever Postgres you're restoring
into.

```bash
# 1. See what's available for the environment you need (prod or test):
RCLONE_CONFIG=/opt/bookeat/backups/rclone.conf rclone lsl \
  r2:book-eat/prod/daily/          # or prod/weekly/, test/daily/, test/weekly/

# 2. Pick a file (most recent = last line, sorted by name = sorted by
#    timestamp since the filename is bookeat-YYYYMMDD-HHMMSS.dump), download it:
RCLONE_CONFIG=/opt/bookeat/backups/rclone.conf rclone copyto \
  r2:book-eat/prod/daily/bookeat-20260723-020000.dump \
  /tmp/restore-from-cloud.dump

# 3. Sanity-check the download before touching any database:
ls -la /tmp/restore-from-cloud.dump   # non-zero size
docker run --rm --network none -v /tmp:/backups:ro postgres:16.6-alpine \
  pg_restore --list /backups/restore-from-cloud.dump | head
#   ^ should print a long list of catalog entries, not an error

# 4. Get the file into the target Postgres container and restore into a NEW
#    database — never restore over the live one directly:
cd /opt/bookeat/deploy   # wherever the target compose stack lives
docker compose cp /tmp/restore-from-cloud.dump postgres:/tmp/restore-from-cloud.dump
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "CREATE DATABASE bookeat_restored OWNER $DB_USERNAME;"
docker compose exec -T postgres pg_restore -U "$DB_USERNAME" \
  -d bookeat_restored --no-owner /tmp/restore-from-cloud.dump

# 5. Check it looks right before doing anything drastic (row counts,
#    a couple of known rows, `information_schema.tables` count).

# 6a. If this is a genuine disaster recovery (old DB is gone/corrupt) and
#     bookeat_restored looks right, promote it:
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "ALTER DATABASE bookeat RENAME TO bookeat_old_broken;"
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "ALTER DATABASE bookeat_restored RENAME TO bookeat;"
docker compose restart app worker   # they hold a connection pool to the old name
curl -fsS http://127.0.0.1/health
#   Only drop bookeat_old_broken once you're sure — keep it a day, not a minute.

# 6b. If this was just a drill/verification, clean up instead:
docker compose exec -T postgres psql -U "$DB_USERNAME" -d postgres \
  -c "DROP DATABASE bookeat_restored;"
docker compose exec -T postgres rm -f /tmp/restore-from-cloud.dump
rm -f /tmp/restore-from-cloud.dump
```

This was rehearsed for real on the test server on 2026-07-23: downloaded a
dump from `r2:book-eat/test/daily/...` (not the local copy), restored into a
throwaway `bookeat_cloud_restore_test` database, compared table count (36 vs
36) and per-table row counts (`information_schema.tables` cross-check,
`diff` came back empty — identical) against the live `bookeat` database, then
dropped the throwaway database. See the deploy report for the exact
transcript.

**Rotating R2 credentials:** update `/home/tai/.bookeat/r2.env` locally
first, then push the same file to `/opt/bookeat/backups/r2.env` (chmod 600)
on both servers, regenerate `/opt/bookeat/backups/rclone.conf` from the new
values (same file, `access_key_id`/`secret_access_key`/`endpoint` lines), and
run the backup script once by hand to confirm the new credentials work
end-to-end before leaving it to cron.
