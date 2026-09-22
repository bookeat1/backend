-- +goose Up

-- MARATHON QR ATTRIBUTION (spec marathon-qr-attribution-20260921, rev 2).
--
-- A permanent, campaign-agnostic channel tag — NOT the promos/promo_codes
-- machinery (§0.1 of the spec explains why a promo row was rejected: it is
-- gate-y, has a hard expiry, and costs a release per new channel). It is a
-- free-form lowercase-kebab string ("tshirt", "box", whatever the Detour
-- dashboard prints next), never FK'd to anything, and it outlives the
-- marathon.
--
-- Two independent writers, two independent lifetimes:
--
--   bookings.attribution_source  — the channel THIS booking was created
--     under. Written only by the INSERT (same convention as promotion_id /
--     promo_code / promo_code_id — see migrations/0004, 0108): a PATCH never
--     rewrites it, so "what channel this booking came from" stays a fact
--     about the moment it was created.
--   users.attribution_source /
--   users.attribution_at         — the account's FIRST touch, written once
--     by POST /api/v1/users/me/attribution (WHERE attribution_source IS
--     NULL — see usecase, a later pass) and never overwritten after. A
--     guest who scans "tshirt" then later "box" keeps "tshirt" as their
--     account's channel even though their next booking carries "box" — the
--     two columns are allowed to disagree (spec §3, ugly case 5).
--
-- CHECK constraints mirror domain.AttributionSourceRe (^[a-z0-9_-]{1,32}$)
-- as a storage-level insurance policy, same convention as
-- promo_codes_code_normalized in migrations/0108 — the application already
-- refuses to write anything else (invalid input becomes NULL, never a
-- rejected booking, spec §4 criterion 9), this just makes a stray manual
-- UPDATE or a future code path impossible to get wrong silently.
--
-- SAFE ON LIVE ROWS: three nullable columns, no default, no backfill — a
-- catalog-only ALTER (metadata rewrite on PG 11+, no table lock beyond the
-- brief DDL lock). Every existing row reads back as NULL, i.e. "no known
-- channel", exactly what it meant before this column existed.
SET lock_timeout = '3s';

ALTER TABLE bookings
    ADD COLUMN attribution_source varchar(32),
    ADD CONSTRAINT bookings_attribution_source_format
        CHECK (attribution_source IS NULL OR attribution_source ~ '^[a-z0-9_-]{1,32}$');

ALTER TABLE users
    ADD COLUMN attribution_source varchar(32),
    ADD COLUMN attribution_at timestamptz,
    ADD CONSTRAINT users_attribution_source_format
        CHECK (attribution_source IS NULL OR attribution_source ~ '^[a-z0-9_-]{1,32}$'),
    ADD CONSTRAINT users_attribution_at_needs_source
        CHECK (attribution_at IS NULL OR attribution_source IS NOT NULL);

COMMENT ON COLUMN bookings.attribution_source IS
  'Marathon QR channel tag (tshirt/box/...) this booking was created under. Written only at INSERT, never by PATCH. NULL = no channel (staff/phone bookings, no scan, or an invalid tag that was silently dropped).';
COMMENT ON COLUMN users.attribution_source IS
  'First-touch channel tag for this account, written once by POST /api/v1/users/me/attribution and never overwritten. May differ from a later bookings.attribution_source (guest scanned a second QR after signup).';
COMMENT ON COLUMN users.attribution_at IS
  'When users.attribution_source was first recorded. NULL exactly when attribution_source is NULL.';

-- Reporting indexes (spec §5/§7, docs/runbook-admin.md §10 "Выгрузки марафона"):
-- every report groups by channel and narrows by a created_at window, so both
-- columns together are what every query filters and sorts on. Partial
-- (WHERE ... IS NOT NULL) because the vast majority of rows carry no tag —
-- an index over that would be mostly dead weight for a query that always
-- excludes NULL anyway (report В "по каждому attribution_source", список
-- А/Б's "непустой attribution_source" condition, spec §5).
CREATE INDEX idx_bookings_attribution_source
    ON bookings (attribution_source, created_at)
    WHERE attribution_source IS NOT NULL;

CREATE INDEX idx_users_attribution_source
    ON users (attribution_source, created_at)
    WHERE attribution_source IS NOT NULL;

-- Список А/Б (spec §4 criteria 24-25) window their whole population by plain
-- registration/creation time BEFORE looking at the channel at all (список Б
-- has no channel condition whatsoever) — neither table had an index on bare
-- created_at before this migration.
CREATE INDEX idx_users_created_at ON users (created_at);
CREATE INDEX idx_bookings_created_at ON bookings (created_at);

-- +goose Down

DROP INDEX IF EXISTS idx_bookings_created_at;
DROP INDEX IF EXISTS idx_users_created_at;
DROP INDEX IF EXISTS idx_users_attribution_source;
DROP INDEX IF EXISTS idx_bookings_attribution_source;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_attribution_at_needs_source,
    DROP CONSTRAINT IF EXISTS users_attribution_source_format,
    DROP COLUMN IF EXISTS attribution_at,
    DROP COLUMN IF EXISTS attribution_source;

ALTER TABLE bookings
    DROP CONSTRAINT IF EXISTS bookings_attribution_source_format,
    DROP COLUMN IF EXISTS attribution_source;
