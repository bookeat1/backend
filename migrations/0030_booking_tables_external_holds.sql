-- +goose Up

-- Make booking_tables the SINGLE physical occupancy layer for BOTH BookEat's
-- own bookings and external holds, so the one GiST exclusion constraint already
-- on it (table_id WITH =, slot WITH &&) WHERE active rejects a slot taken by a
-- booking OR by an external hold — atomically, with no lost-race window. Before
-- this, external holds lived only in external_reservations and could not conflict
-- with a booking at the database level; a check would lose the race, a constraint
-- does not.
--
-- Each row is owned by EXACTLY ONE of a booking or an external reservation.
-- Existing rows are all booking-owned (booking_id NOT NULL,
-- external_reservation_id NULL), so the new CHECK holds for every live row and
-- the DROP NOT NULL is metadata-only — safe on a populated table.
ALTER TABLE booking_tables
    ALTER COLUMN booking_id DROP NOT NULL;

ALTER TABLE booking_tables
    ADD COLUMN external_reservation_id uuid
        REFERENCES external_reservations (id) ON DELETE CASCADE;

-- Exactly one owner. `<>` on two booleans is XOR: true iff precisely one side is
-- set. Deleting the external_reservations row cascades its enforcement rows away
-- here, which frees the slot.
ALTER TABLE booking_tables
    ADD CONSTRAINT booking_tables_one_owner
        CHECK ((booking_id IS NOT NULL) <> (external_reservation_id IS NOT NULL));

CREATE INDEX idx_booking_tables_external ON booking_tables (external_reservation_id)
    WHERE external_reservation_id IS NOT NULL;

-- The status trigger that maintains booking_tables.active keys off booking_id
-- (WHERE booking_id = NEW.id), so external-owned rows (booking_id NULL) are
-- never touched by it — their active flag is managed by the hold's own lifecycle
-- (true on insert, gone on delete). No trigger change is needed.

-- +goose Down

-- Remove external-owned rows first so booking_id can go back to NOT NULL.
DELETE FROM booking_tables WHERE external_reservation_id IS NOT NULL;

DROP INDEX idx_booking_tables_external;
ALTER TABLE booking_tables DROP CONSTRAINT booking_tables_one_owner;
ALTER TABLE booking_tables DROP COLUMN external_reservation_id;
ALTER TABLE booking_tables ALTER COLUMN booking_id SET NOT NULL;
