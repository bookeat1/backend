-- +goose Up

-- BookEat is only one funnel. A table can be taken by a phone call, a walk-in,
-- or an external POS / booking system (a future Kwaaka webhook), and the
-- availability engine must never resell a slot already occupied elsewhere.
--
-- external_reservations is the source-of-record for that outside occupancy: one
-- row per hold, source-agnostic (manual / phone / walkin / pos / kwaaka). It is
-- the ingestion seam a future POS webhook writes into — the same shape a staff
-- member fills in by hand today. Enumerated fields are VARCHAR, validated in Go
-- (see internal/domain/external_reservation.go), never a Postgres ENUM.
--
-- The PHYSICAL exclusion that actually stops a double-booking across sources is
-- NOT here — it is the existing GiST constraint on booking_tables. This table
-- carries the audit/record fields; migration 0030 wires each per-table hold into
-- booking_tables so one constraint guards bookings and external holds alike.
--
-- table_id NULL = a whole-venue block (private event, kitchen closed): it is
-- expanded to one booking_tables row per active table at creation time (see the
-- usecase), so it is enforced by the same constraint for every table that exists
-- at that moment.
CREATE TABLE external_reservations
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    -- NULL = whole-venue block; otherwise the specific table it occupies.
    table_id      uuid        REFERENCES restaurant_tables (id) ON DELETE CASCADE,
    slot          tstzrange   NOT NULL,
    source        varchar     NOT NULL,
    -- external_ref is the other system's own id for this reservation: the dedup
    -- key that makes a retried / duplicated POS webhook idempotent.
    external_ref  varchar,
    note          varchar,
    -- created_by is the staff member who entered a manual hold; NULL for a hold
    -- ingested by a machine (a webhook has no user).
    created_by    uuid        REFERENCES users (id) ON DELETE SET NULL,
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (upper(slot) > lower(slot))
);

-- Two active per-table holds for the same table may not overlap — the same
-- guarantee booking_tables gives bookings, kept for holds among themselves.
-- (btree_gist, needed to mix `=` on a scalar with `&&` on a range, is already
-- installed by migration 0004.)
ALTER TABLE external_reservations
    ADD CONSTRAINT external_reservations_no_table_overlap
        EXCLUDE USING gist (table_id WITH =, slot WITH &&) WHERE (active AND table_id IS NOT NULL);

-- Two active whole-venue blocks for the same venue may not overlap either.
ALTER TABLE external_reservations
    ADD CONSTRAINT external_reservations_no_venue_overlap
        EXCLUDE USING gist (restaurant_id WITH =, slot WITH &&) WHERE (active AND table_id IS NULL);

-- Idempotent ingestion: one active hold per (restaurant, source, external_ref).
-- A POS resending the same reservation id updates nothing and inserts nothing
-- twice. Partial so hand-entered holds (external_ref NULL) are never deduped.
CREATE UNIQUE INDEX idx_external_reservations_dedup
    ON external_reservations (restaurant_id, source, external_ref)
    WHERE active AND external_ref IS NOT NULL;

CREATE INDEX idx_external_reservations_table ON external_reservations (table_id) WHERE active;
CREATE INDEX idx_external_reservations_restaurant_slot
    ON external_reservations USING gist (restaurant_id, slot) WHERE active;

-- +goose Down
DROP TABLE external_reservations;
