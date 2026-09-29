-- +goose Up

-- Per-venue loyalty/bonus-program toggle (Trello ask, 2026-09-29). The mobile
-- app already ships a "loyalty QR code" button on the venue screen (frontend
-- PR #275) that is PURELY visual — there is no loyalty engine behind it, and
-- this migration does not add one. It only gives the admin panel a per-venue
-- switch the mobile app can read to decide whether that button should show at
-- all, mirroring is_active/is_premium/is_popular: one BOOLEAN column on
-- restaurants, no separate table.
--
-- DEFAULT false, same reasoning as booking_payment_required in migration
-- 0036: safe on a table with live rows (every existing venue keeps its
-- current behaviour, no backfill needed), and false is the only sane default
-- for a feature that does not actually work yet — it must never come on for a
-- venue by accident.
ALTER TABLE restaurants
    ADD COLUMN loyalty_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE restaurants
    DROP COLUMN loyalty_enabled;
