-- +goose Up

-- booking_tables.active DERIVED FROM THE BOOKING STATUS ON INSERT — the mirror
-- of what 0057 did for booking_capacity_holds, and needed for the same reason.
--
-- CONTEXT. Switching a venue from `seats` (table-less) mode back to `tables`
-- mode now reconciles its existing table-less bookings against the table engine:
-- each of them is given real booking_tables rows inside the same locked
-- transaction, so the GiST exclusion constraint becomes authoritative for them
-- too (without that reconciliation the room read empty at a time a confirmed
-- party was already sitting there, and the same seats were sold twice — review
-- finding, 25.07.2026).
--
-- That reconciliation has to place WAITLISTED bookings as well. A waitlisted
-- booking holds nothing today, but confirming it re-claims the space, and the
-- re-claim is expressed as the status trigger from 0004 flipping its links back
-- to active — a booking with no links at all flips nothing and would come back
-- from the waiting list confirmed and invisible to the exclusion constraint,
-- which is exactly hole (2) that 0057 closed on the capacity side.
--
-- Inserting links for a waitlisted booking was impossible to do correctly from
-- Go: the column defaults to true, so the links would BLOCK the table for a
-- booking that holds nothing, and writing `active` from application code is
-- forbidden by 0004's own rule ("active is maintained exclusively by the
-- trigger"). So the database derives it, once, at insert time.
--
-- External holds (0030: booking_id NULL, external_reservation_id set) are left
-- exactly as they are — their `active` is their own lifecycle's business, true
-- on insert and gone on delete, and there is no booking row to ask.
--
-- SAFE ON A POPULATED DATABASE: one function and one BEFORE INSERT trigger. No
-- existing row is read or rewritten, and for every caller that inserts links for
-- a LIVE booking (create, amend) the derived value is the same `true` the
-- default produced.

-- +goose StatementBegin
CREATE FUNCTION booking_table_active() RETURNS trigger AS
$$
BEGIN
    IF NEW.booking_id IS NULL THEN
        -- External hold: no booking owns it, so nothing to derive from.
        RETURN NEW;
    END IF;
    -- The booking always exists: booking_id is a FK, and every insert path
    -- writes the booking earlier in the same transaction.
    SELECT (b.status IN ('pending', 'confirmed', 'arrived'))
    INTO NEW.active
    FROM bookings b
    WHERE b.id = NEW.booking_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_booking_tables_active_default
    BEFORE INSERT
    ON booking_tables
    FOR EACH ROW
EXECUTE FUNCTION booking_table_active();

COMMENT ON COLUMN booking_tables.active IS
    'Whether this link currently occupies the table (and is therefore seen by the GiST exclusion constraint). Never written by application code: derived from the booking status on INSERT (0058) and flipped by the status trigger (0004). Rows owned by an external reservation keep the value their own lifecycle sets.';

-- +goose Down
DROP TRIGGER trg_booking_tables_active_default ON booking_tables;
DROP FUNCTION booking_table_active();
