-- +goose Up

-- DELIBERATE OVERBOOKING IN TABLE-LESS MODE (owner decision, 25.07.2026).
--
-- WHY THIS EXISTS. Migration 0054 gave capacity mode a DB-level overbooking
-- guarantee (restaurant_capacity_buckets + CHECK seats_taken <= seats_limit) and
-- then refused `force` outright, because a forced booking carrying no holds
-- would be invisible to that CHECK. But "we'll bring a chair" is a real thing in
-- a restaurant: staff must be able to seat a party the declared capacity does
-- not fit. This migration adds the AUDIT half of that; the ledger half needs no
-- schema change at all, and the reason is worth writing down here because it is
-- the least obvious part of the design:
--
--   seats_limit on a bucket row is DENORMALISED and re-stamped by every
--   INCREMENT (apply_capacity_delta sets seats_limit = p_limit on conflict),
--   and p_limit comes from the hold being inserted. So an override booking
--   simply stamps its holds with `already_taken + party_size` for the buckets it
--   overflows: the seats it takes are counted in seats_taken like anybody
--   else's, the CHECK still holds, and the bucket ends up with ZERO headroom.
--   Because the limit lives on the bucket and not on the venue, nothing about
--   the venue's declared capacity changes, and because the NEXT increment
--   re-stamps seats_limit from ITS hold (which carries the plain declared
--   capacity), the raise cannot be inherited by the following booking — that one
--   trips the CHECK exactly as before. Releasing an overridden booking is the
--   ordinary decrement path, unchanged.
--
-- What could NOT be done instead, for the record: exempting override holds from
-- the CHECK (that is the invisible-booking hole 0054 exists to close), or
-- raising restaurants.booking_capacity_seats (that would silently sell the extra
-- seats to every future guest, and someone would have to remember to lower it
-- back).
--
-- SAFE ON A POPULATED DATABASE: one brand-new table, no change to any existing
-- table, no backfill.

-- Append-only audit of every deliberate overbooking. One row per booking (the
-- UNIQUE below is what makes the write exactly-once even under a retry), and it
-- carries enough numbers to answer the owner's question without a join:
-- "who put 12 people into a 10-seat room last Saturday".
CREATE TABLE booking_capacity_overrides
(
    id            uuid PRIMARY KEY,
    -- ON DELETE RESTRICT, not CASCADE: an audit row that disappears with the
    -- thing it audits is not an audit trail. Same choice payments made in 0007.
    booking_id    uuid        NOT NULL UNIQUE REFERENCES bookings (id) ON DELETE RESTRICT,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE RESTRICT,
    -- WHO. NOT NULL and RESTRICT: "some manager did it" is not an answer, and
    -- nothing in this product hard-deletes a user.
    actor_user_id uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    actor_type    varchar     NOT NULL CHECK (actor_type IN ('manager', 'admin')),
    -- The party that was seated and the capacity that was declared at that
    -- moment. Stored rather than derived: the venue may change its declared
    -- capacity afterwards, and the audit must keep saying what the room was.
    guests           integer     NOT NULL CHECK (guests > 0),
    declared_seats   integer     NOT NULL CHECK (declared_seats > 0),
    -- HOW FAR OVER, and where. seats_over is the WORST overshoot across the
    -- booking's buckets (a visit spans ~10 of them and each may be over by a
    -- different amount); peak_bucket_start/peak_seats_taken name that bucket and
    -- the total that ended up in it. > 0 by construction: a booking that fits is
    -- not an override and writes no row here.
    seats_over        integer     NOT NULL CHECK (seats_over > 0),
    peak_bucket_start timestamptz NOT NULL,
    peak_seats_taken  integer     NOT NULL CHECK (peak_seats_taken > 0),
    created_at        timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE booking_capacity_overrides IS
    'Append-only record of every booking staff deliberately seated beyond the venue declared capacity (table-less mode). Never updated, never deleted.';

-- The venue owner's two questions: "what happened last Saturday" (by time) and
-- "what happened around 20:00" (by the bucket that was overloaded).
CREATE INDEX idx_capacity_overrides_venue_created ON booking_capacity_overrides (restaurant_id, created_at DESC);
CREATE INDEX idx_capacity_overrides_venue_bucket ON booking_capacity_overrides (restaurant_id, peak_bucket_start);
-- And the platform's question: "which member of staff does this repeatedly".
CREATE INDEX idx_capacity_overrides_actor ON booking_capacity_overrides (actor_user_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION forbid_capacity_override_rewrite() RETURNS trigger AS
$$
BEGIN
    -- Append-only enforced in the database, not by convention. An audit trail
    -- the application can quietly rewrite is worth nothing in the argument it
    -- exists to settle. The table holds no guest personal data (staff id and
    -- counts only), so no erasure request ever needs to reach it; a deliberate
    -- DBA purge can still drop this trigger, which is exactly the amount of
    -- friction that action deserves.
    RAISE EXCEPTION 'booking_capacity_overrides is append-only (attempted %)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_capacity_overrides_append_only
    BEFORE UPDATE OR DELETE
    ON booking_capacity_overrides
    FOR EACH ROW
EXECUTE FUNCTION forbid_capacity_override_rewrite();

-- +goose Down
DROP TRIGGER trg_capacity_overrides_append_only ON booking_capacity_overrides;
DROP FUNCTION forbid_capacity_override_rewrite();
DROP TABLE booking_capacity_overrides;
