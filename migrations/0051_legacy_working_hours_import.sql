-- +goose Up

-- legacy_working_hours_import records what the one-way legacy sync did with a
-- venue's free-text opening hours (restaurants.opening_hours, the only place the
-- OLD system keeps them). One row per restaurant.
--
-- It exists to answer one question safely: MAY the sync write this venue's
-- weekly rows, or has a human taken them over?
--
--   * status='filled'      — the seven restaurant_working_hours rows were written
--                            by the sync. `fingerprint` is a content hash of
--                            exactly what it wrote; the sync refreshes those rows
--                            only while they still hash to that value, i.e. while
--                            nobody has edited them in the admin panel.
--   * status='venue_owned' — hours exist that the sync did not write (entered by
--                            hand), or they no longer match the fingerprint. The
--                            venue's own edit always wins: the sync never touches
--                            them again.
--   * status='refused'     — the free text could not be parsed into a schedule
--                            (`reason` says why). Nothing was written; the venue
--                            must be filled in by hand. Recorded so the same
--                            unparseable text is not re-reported every tick.
--
-- source_text is the legacy string as it was when the decision was taken. The
-- sync reconsiders a venue only when restaurants.opening_hours differs from it,
-- which makes the whole pass idempotent and cheap to re-run.
--
-- Nothing in the request path reads this table; it is written only by
-- cmd/worker's legacy-sync loop and stays empty when LEGACY_DB_URL is unset.
CREATE TABLE legacy_working_hours_import
(
    restaurant_id uuid PRIMARY KEY REFERENCES restaurants (id) ON DELETE CASCADE,
    source_text   text        NOT NULL,
    status        varchar     NOT NULL CHECK (status IN ('filled', 'venue_owned', 'refused')),
    -- Set for 'filled' only: the hash of the rows the sync wrote. The CHECK is
    -- the guard that makes the "never overwrite a venue's own edit" rule
    -- impossible to bypass by writing a 'filled' record with no fingerprint to
    -- compare against.
    fingerprint   text,
    reason        text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT legacy_working_hours_import_fingerprint_required
        CHECK ((status = 'filled') = (fingerprint IS NOT NULL))
);

-- +goose Down
DROP TABLE legacy_working_hours_import;
