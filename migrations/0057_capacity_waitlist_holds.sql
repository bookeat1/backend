-- +goose Up

-- WAITLISTED BOOKINGS AND THE CAPACITY GUARANTEE (review finding, 25.07.2026).
--
-- A waitlisted booking releases its seats (the status trigger from 0054 flips
-- its holds to active=false) and re-claims them when it is confirmed. That
-- re-claim was judged against the WRONG number, and in one case there was
-- nothing to re-claim at all. Both halves were reachable through the product
-- API, silently, with no audit row:
--
--   (1) the reactivation branch of sync_capacity_bucket passed the hold row's
--       OWN seats_limit — a snapshot of the venue's capacity as it was when the
--       hold was written. A venue that lowered its capacity while the booking
--       sat on the waiting list (allowed: a waitlisted booking counts nothing,
--       so nothing blocks the reduction) got the OLD limit re-stamped onto the
--       bucket on confirmation, and the room was silently oversold. Reproduced
--       by the reviewer: 10-seat venue → booking of 4 waitlisted → capacity
--       lowered to 5 → another booking fills 5/5 → confirm the waitlisted one →
--       bucket reads 9/10 in a five-seat room.
--
--   (2) worse, a booking waitlisted BEFORE a tables→seats switch got no holds
--       from the mode-switch backfill (which walks only the statuses that hold a
--       table), so confirming it later flipped zero rows: a confirmed booking
--       holding nothing, invisible to the ledger for its whole life.
--
-- The fixes are one idea each, and they compose.
--
-- FIX 1 — a reactivation is judged by TODAY's rules. Re-claiming a seat is a new
-- claim on the room, not a replay of an old one, so the limit it is measured
-- against is the venue's CURRENT declared capacity, read here rather than taken
-- from the hold. A confirmation that no longer fits now fails the CHECK and
-- aborts the status transition — which is the honest outcome and the one 0054
-- already documented for this branch. Note this also means an OVERBOOKED
-- booking (0056) that was waitlisted cannot quietly re-claim its extra seats:
-- putting people back into a full room is a fresh deliberate decision and needs
-- a fresh, audited one.
--
-- FIX 2 — a hold row's `active` is derived from the booking's status BY THE
-- DATABASE, at insert time, instead of defaulting to true and hoping the caller
-- only ever inserts holds for live bookings. That keeps 0054's rule ("`active`
-- is the trigger's business, never Go's") literally true while letting the
-- mode-switch backfill write holds for a waitlisted booking: they cost nothing
-- until it is confirmed, and confirming it then goes through the same
-- reactivation path as everybody else — counted, and judged by fix 1.
--
-- SAFE ON A POPULATED DATABASE: two function bodies and one BEFORE trigger. No
-- table is touched, no row is rewritten, no lock beyond the DDL itself.

-- +goose StatementBegin
CREATE FUNCTION booking_capacity_hold_active() RETURNS trigger AS
$$
BEGIN
    -- The booking always exists: booking_id is a NOT NULL FK, and the create
    -- path inserts the booking earlier in the same transaction.
    SELECT (b.status IN ('pending', 'confirmed', 'arrived'))
    INTO NEW.active
    FROM bookings b
    WHERE b.id = NEW.booking_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_booking_capacity_holds_active_default
    BEFORE INSERT
    ON booking_capacity_holds
    FOR EACH ROW
EXECUTE FUNCTION booking_capacity_hold_active();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sync_capacity_bucket() RETURNS trigger AS
$$
DECLARE
    v_limit integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.active THEN
            PERFORM apply_capacity_delta(NEW.restaurant_id, NEW.bucket_start, NEW.seats, NEW.seats_limit);
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.active THEN
            PERFORM apply_capacity_delta(OLD.restaurant_id, OLD.bucket_start, -OLD.seats, OLD.seats_limit);
        END IF;
    ELSIF OLD.active AND NOT NEW.active THEN
        PERFORM apply_capacity_delta(OLD.restaurant_id, OLD.bucket_start, -OLD.seats, OLD.seats_limit);
    ELSIF NOT OLD.active AND NEW.active THEN
        -- Re-activation (waitlist → confirmed) is a real new claim on the
        -- capacity, so it is measured against what the venue says it can seat
        -- TODAY — not against the limit frozen into the hold when it was
        -- written, which may predate a capacity reduction or belong to a
        -- deliberate overbooking. It can legitimately fail the CHECK if the
        -- venue filled up meanwhile; the status transition then aborts, which is
        -- the honest outcome.
        SELECT r.booking_capacity_seats INTO v_limit
        FROM restaurants r
        WHERE r.id = NEW.restaurant_id;
        -- NULL only for a venue that is no longer configured for capacity
        -- bookings at all; nothing better is known then than what the hold says.
        IF v_limit IS NULL THEN
            v_limit := NEW.seats_limit;
        END IF;
        PERFORM apply_capacity_delta(NEW.restaurant_id, NEW.bucket_start, NEW.seats, v_limit);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

COMMENT ON COLUMN booking_capacity_holds.active IS
    'Whether this hold currently counts against the venue capacity. Never written by application code: set from the booking status on INSERT (0057) and flipped by the status trigger (0054).';

-- +goose Down
DROP TRIGGER trg_booking_capacity_holds_active_default ON booking_capacity_holds;
DROP FUNCTION booking_capacity_hold_active();

-- Restore the 0054 body verbatim.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sync_capacity_bucket() RETURNS trigger AS
$$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.active THEN
            PERFORM apply_capacity_delta(NEW.restaurant_id, NEW.bucket_start, NEW.seats, NEW.seats_limit);
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.active THEN
            PERFORM apply_capacity_delta(OLD.restaurant_id, OLD.bucket_start, -OLD.seats, OLD.seats_limit);
        END IF;
    ELSIF OLD.active AND NOT NEW.active THEN
        PERFORM apply_capacity_delta(OLD.restaurant_id, OLD.bucket_start, -OLD.seats, OLD.seats_limit);
    ELSIF NOT OLD.active AND NEW.active THEN
        PERFORM apply_capacity_delta(NEW.restaurant_id, NEW.bucket_start, NEW.seats, NEW.seats_limit);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
