-- +goose Up

-- Restaurant admin panel (Ф1): special-day schedule overrides. A restaurant's
-- REGULAR weekly hours already live in the working_hours table (one row per
-- day_of_week). This table holds ONLY the exceptions to that schedule for a
-- specific calendar date — a holiday closure or a one-off change of hours — so
-- a date with no row here simply follows the weekly schedule. Kept as a
-- separate table (not JSON on restaurants) so the availability engine can join
-- and index it, and so an override is a first-class, auditable row.
--
-- Safe on a table that already has live rows: this is a brand-new table with a
-- foreign key to the existing restaurants table, no change to any existing
-- table, so nothing to backfill and no rewrite lock on live data.
--
-- is_closed = true  → the venue is shut that whole day; open_time/close_time
--                     are NULL and ignored.
-- is_closed = false → open with DIFFERENT hours than usual; both times are set.
-- The CHECK encodes exactly that invariant so a malformed override cannot be
-- stored. Times are 'HH:MM' text in the venue's own timezone, matching
-- working_hours.open_time/close_time (which are also stored as text there).
CREATE TABLE restaurant_schedule_overrides (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    override_date date        NOT NULL,
    is_closed     boolean     NOT NULL DEFAULT false,
    open_time     varchar,
    close_time    varchar,
    note          varchar,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT restaurant_schedule_overrides_hours_check
        CHECK (
            (is_closed = true  AND open_time IS NULL AND close_time IS NULL)
            OR
            (is_closed = false AND open_time IS NOT NULL AND close_time IS NOT NULL)
        )
);

-- One override per (restaurant, day): makes "set the override for this day"
-- idempotent (Upsert / ON CONFLICT) and lookups by day a single index probe.
CREATE UNIQUE INDEX idx_schedule_overrides_restaurant_date
    ON restaurant_schedule_overrides (restaurant_id, override_date);

-- +goose Down
DROP INDEX IF EXISTS idx_schedule_overrides_restaurant_date;
DROP TABLE IF EXISTS restaurant_schedule_overrides;
