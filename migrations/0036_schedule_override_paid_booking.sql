-- +goose Up

-- Paid booking on special days. A restaurant's special-day exceptions already
-- live in restaurant_schedule_overrides (migration 0024): a holiday closure or
-- a one-off change of hours for a specific calendar date. These two columns
-- extend that same override so a venue can additionally mark a given day as
-- PAID — the guest must place a prepayment (deposit) to book on that date —
-- and set the deposit amount. Bookings are FREE by default (a normal day, or a
-- special-day override that leaves booking_payment_required = false); only a
-- day explicitly marked here requires prepayment.
--
--   booking_payment_required = false → the default: no prepayment for this day
--                                      beyond whatever the restaurant's own
--                                      payment settings already require.
--   booking_payment_required = true  → this special day is PAID; the guest must
--                                      prepay deposit_amount_minor to book.
--
-- deposit_amount_minor is int64 MINOR units (tiyn for KZT), never a float,
-- matching every other money column in this schema. The CHECK encodes the
-- invariant so a malformed override cannot be stored:
--   * it is NULL or non-negative (never a negative amount), and
--   * when booking_payment_required is true it must be present and > 0 (a paid
--     day with a zero/absent deposit is a misconfiguration, not a free day).
--
-- Safe on a table that already has live rows: booking_payment_required is added
-- with a NOT NULL DEFAULT false, so every existing override keeps its current
-- (free) behaviour with no backfill needed; deposit_amount_minor is nullable
-- and defaults to NULL. The CHECK is satisfied by every pre-existing row
-- (false + NULL), so ADD CONSTRAINT does not rewrite or reject live data.
ALTER TABLE restaurant_schedule_overrides
    ADD COLUMN booking_payment_required boolean NOT NULL DEFAULT false,
    ADD COLUMN deposit_amount_minor     bigint,
    ADD CONSTRAINT restaurant_schedule_overrides_deposit_check
        CHECK (
            (deposit_amount_minor IS NULL OR deposit_amount_minor >= 0)
            AND
            (booking_payment_required = false OR (deposit_amount_minor IS NOT NULL AND deposit_amount_minor > 0))
        );

-- +goose Down
ALTER TABLE restaurant_schedule_overrides
    DROP CONSTRAINT restaurant_schedule_overrides_deposit_check,
    DROP COLUMN deposit_amount_minor,
    DROP COLUMN booking_payment_required;
