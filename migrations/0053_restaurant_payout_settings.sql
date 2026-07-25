-- +goose Up

-- PER-VENUE PAYOUT POLICY (owner decision, 25.07.2026).
--
-- Until now the payout policy was platform-wide and env-only: one threshold
-- (PAYOUTS_MIN_AMOUNT_MINOR, 10 000 ₸) for every venue. That threshold stays
-- the platform DEFAULT and stays configurable by env — this migration only adds
-- the ability for a single venue to deviate from it, plus the safety valve the
-- threshold on its own never had.
--
-- The safety valve is max_hold_days. A venue whose daily turnover never reaches
-- the threshold would otherwise roll over forever and its money would sit with
-- us indefinitely. Past that many whole venue-local days, the next pass pays out
-- regardless of the threshold, accepts the acquirer's fee, and marks the payout
-- as forced by age.
--
-- WHY A SEPARATE TABLE, NOT COLUMNS ON `restaurants`:
--   * `restaurants` is the hottest read in the product (feed, search, booking).
--     A money knob read once per venue per day does not belong in the row that
--     every guest-facing query pulls.
--   * The payout module already owns restaurant_payout_destinations. Keeping its
--     second venue-scoped table alongside it means the payouts repository never
--     needs write access to `restaurants` — the module boundary stays a boundary.
--   * The three-state semantics need a home: no ROW = this venue follows the
--     platform in everything; a row with a NULL column = it follows the platform
--     for THAT knob. Nullable columns on `restaurants` would express the second
--     but not the first, and would make "which venues have a custom policy" a
--     scan instead of a table.
--   * Audit (updated_by/updated_at) belongs to the settings, not to the venue.
--
-- Safe on a populated database: a brand-new table (so no existing row is
-- touched, no lock on `restaurants`), and the one added column on `payouts` is
-- NOT NULL with a DEFAULT false, which is the truth for every historical payout
-- — none of them could have been forced by a rule that did not exist.

CREATE TABLE restaurant_payout_settings
(
    -- One row per venue: the restaurant IS the key, so a duplicate policy for
    -- the same venue is impossible by construction rather than by convention.
    restaurant_id    uuid PRIMARY KEY REFERENCES restaurants (id) ON DELETE CASCADE,
    -- This venue's own payout threshold in minor units. NULL = follow the
    -- platform default (PAYOUTS_MIN_AMOUNT_MINOR). 0 is a legitimate, distinct
    -- value meaning "pay any positive balance", which is why this is nullable
    -- rather than defaulted.
    min_payout_minor bigint,
    -- After this many whole venue-local days of holding the venue's OLDEST
    -- unpaid money, the next pass pays out even below the threshold. NULL =
    -- follow the platform default (PAYOUTS_MAX_HOLD_DAYS, 7 days). 0 = never
    -- force, roll over indefinitely.
    max_hold_days    integer,
    -- The superadmin who last wrote this policy. ON DELETE SET NULL: losing the
    -- author must never delete the policy itself. Nullable also covers a row
    -- written by an operational script rather than a person.
    updated_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- The bounds mirror domain.PayoutSettings.Validate. They are here as well
    -- as in Go because this table is exactly the kind of row an operator edits
    -- by hand at 2am, and a stray extra zero in the threshold would strand a
    -- venue's money behind a number it can never reach.
    CONSTRAINT chk_payout_settings_min_amount
        CHECK (min_payout_minor IS NULL OR (min_payout_minor >= 0 AND min_payout_minor <= 100000000)),
    CONSTRAINT chk_payout_settings_hold_days
        CHECK (max_hold_days IS NULL OR (max_hold_days >= 0 AND max_hold_days <= 365))
);

COMMENT ON TABLE restaurant_payout_settings IS
    'Per-venue overrides of the platform payout policy. A missing row, or a NULL column, means the venue follows the platform default. Written by superadmins only.';

ALTER TABLE payouts
    -- TRUE when this payout was produced by the max-hold rule rather than by the
    -- threshold: the venue was still below its minimum, but its oldest unpaid
    -- money had waited long enough. It still paid the acquirer's fee — that is
    -- the point of the setting — so the statement has to say WHY it happened
    -- instead of leaving a venue to wonder why a small payout cost 300 ₸.
    ADD COLUMN forced_by_age boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE payouts
    DROP COLUMN forced_by_age;
DROP TABLE restaurant_payout_settings;
