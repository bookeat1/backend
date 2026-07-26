-- +goose Up

-- LOCK SAFETY. This migration takes ACCESS EXCLUSIVE on `restaurants` (ALTER
-- TABLE … ADD COLUMN) and SHARE ROW EXCLUSIVE on `bookings` (CREATE TRIGGER).
-- The first conflicts with every reader and writer of the venue catalog, the
-- second with every writer of `bookings` — so both queue behind any transaction
-- still holding a lock on those tables, and while queued they block the traffic
-- piling up behind THEM. On a live database that is a booking outage lasting as
-- long as the oldest open transaction. With a lock_timeout the migration gives
-- up after 3s instead: the deploy fails loudly, traffic never stops, and the
-- migration is simply retried at a quieter moment. RESET keeps the timeout from
-- leaking into the migrations that run after this one in the same session.
SET lock_timeout = '3s';

-- TABLE-LESS ("capacity") BOOKING MODE (owner decision, 25.07.2026).
--
-- WHY THIS EXISTS. The new backend seats a guest at a SPECIFIC table: a booking
-- is only accepted when the availability engine finds a free row in
-- restaurant_tables and writes the corresponding booking_tables link. The old
-- product treated a booking as a REQUEST — the venue sorted out the seating
-- itself — so venues never had a reason to enter their tables. On the live test
-- server only 7 of 31 venues have any restaurant_tables rows at all, and for the
-- other 24 EVERY slot of the availability endpoint comes back
-- available=false / reason=capacity. They are unbookable, and no amount of
-- working-hours import fixes that.
--
-- So a venue may now declare a TOTAL CAPACITY instead of a table list. The unit
-- is SIMULTANEOUS GUESTS (seats), not "tables" and not "parties": the booking
-- request carries a party size, so guests is the only unit that composes —
-- N tables of unknown size cannot answer "does a party of 6 still fit?".
--
-- HOW OVERBOOKING IS PREVENTED WITHOUT TABLES. Table mode leans on a GiST
-- exclusion constraint (booking_tables): the database, not the application,
-- refuses the second booking of the same table. A capacity limit is a SUM, and
-- no exclusion constraint can express a sum — so this migration gives capacity
-- mode its own DB-level guarantee of the same strength:
--
--   * time is discretised into fixed 15-minute buckets;
--   * restaurant_capacity_buckets holds ONE row per (venue, bucket) with the
--     seats already sold in it — that row is the serialisation point, and it
--     exists precisely so that two concurrent bookings have something to
--     contend on;
--   * a CHECK constraint on that row (seats_taken <= seats_limit) is what
--     actually refuses the overbooking. A concurrent INSERT ... ON CONFLICT DO
--     UPDATE blocks on the row lock, then re-applies its delta on top of the
--     COMMITTED value and trips the CHECK. A capacity check written in Go
--     between two queries loses that race; a CHECK does not.
--
-- Why 15 minutes: every real-world timezone offset is a whole multiple of 15
-- minutes, so buckets floored in UTC line up with local quarter-hours too, and a
-- 2h visit plus buffers costs ~10 rows. The bucketing rounds a booking's
-- occupancy window OUTWARDS (a visit from 19:07 occupies the 19:00 bucket in
-- full), so the count can over-reserve by less than a quarter of an hour but can
-- never under-reserve. Conservative in the only direction that is safe.
--
-- SAFE ON A POPULATED DATABASE: two brand-new tables, plus two NULLABLE columns
-- on restaurants (no rewrite, no default to backfill). The CHECK constraints on
-- restaurants are trivially satisfied by the NULLs they are added with, and
-- `restaurants` holds tens of rows, so the validating scan is instant.

ALTER TABLE restaurants
    -- NULL = the platform default = 'tables', i.e. behave exactly as before.
    -- Deliberately NULLABLE rather than NOT NULL DEFAULT 'tables': it matches
    -- every other per-venue booking-policy column added in 0004 (NULL means
    -- "inherit"), and it keeps "which venues made a deliberate choice"
    -- answerable without comparing against a default.
    ADD COLUMN booking_capacity_mode  varchar,
    -- Simultaneous guests the venue can seat. Meaningful only in 'seats' mode.
    ADD COLUMN booking_capacity_seats integer;

ALTER TABLE restaurants
    ADD CONSTRAINT chk_restaurants_capacity_mode
        CHECK (booking_capacity_mode IS NULL OR booking_capacity_mode IN ('tables', 'seats')),
    -- Upper bound 2000. The largest banquet halls in Almaty/Astana seat on the
    -- order of a thousand people at once, so 2000 cannot reject a real venue,
    -- while it does catch the two mistakes this field will actually see: a
    -- stray extra zero and a phone number pasted into the wrong box. The bound
    -- is mirrored in usecase/bookings (maxCapacitySeats) — this copy exists
    -- because this is exactly the kind of row an operator edits by hand.
    ADD CONSTRAINT chk_restaurants_capacity_seats
        CHECK (booking_capacity_seats IS NULL OR (booking_capacity_seats > 0 AND booking_capacity_seats <= 2000)),
    -- A venue in 'seats' mode without a declared capacity would be as
    -- unbookable as the 24 venues this feature exists to fix. Make that state
    -- unrepresentable instead of handling it everywhere in Go.
    ADD CONSTRAINT chk_restaurants_capacity_seats_required
        CHECK (booking_capacity_mode IS DISTINCT FROM 'seats' OR booking_capacity_seats IS NOT NULL);

COMMENT ON COLUMN restaurants.booking_capacity_mode IS
    'tables = seat the guest at a specific restaurant_tables row (default, NULL means this); seats = check the declared total capacity instead, staff assign the table themselves.';

-- The counter, one row per venue per 15-minute bucket. seats_limit is
-- DENORMALISED here on purpose: a CHECK constraint cannot read another table,
-- and the whole guarantee rests on this being a plain declarative CHECK.
--
-- seats_limit is refreshed by every INCREMENT (so raising a venue's capacity
-- takes effect on the next booking) and deliberately left alone by every
-- DECREMENT. Otherwise a venue that lowered its capacity below what is already
-- sold could no longer CANCEL those bookings: the decrement would still leave
-- seats_taken above the new limit and the CHECK would refuse it. Since a
-- decrement can only lower seats_taken, skipping the limit refresh there keeps
-- the invariant "seats_taken <= seats_limit" true after every statement.
CREATE TABLE restaurant_capacity_buckets
(
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    bucket_start  timestamptz NOT NULL,
    seats_taken   integer     NOT NULL,
    seats_limit   integer     NOT NULL,
    PRIMARY KEY (restaurant_id, bucket_start),
    -- THE overbooking guarantee. Do not relax, do not make it a trigger.
    CONSTRAINT chk_capacity_bucket_within_limit
        CHECK (seats_taken >= 0 AND seats_taken <= seats_limit)
);

COMMENT ON TABLE restaurant_capacity_buckets IS
    'Seats sold per venue per 15-minute bucket in table-less mode. The CHECK on seats_taken is what refuses an overbooking; the row is also the lock two concurrent bookings contend on.';

-- Per-booking detail: what this booking holds, in which bucket, under which
-- limit. It is the audit trail AND the source for the release on cancellation —
-- the counter alone could never be unwound. `active` mirrors booking_tables:
-- written exclusively by the trigger on bookings.status, never from Go.
CREATE TABLE booking_capacity_holds
(
    id            uuid PRIMARY KEY,
    booking_id    uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    bucket_start  timestamptz NOT NULL,
    seats         integer     NOT NULL CHECK (seats > 0),
    seats_limit   integer     NOT NULL CHECK (seats_limit > 0),
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- One hold per booking per bucket: a retry that re-inserts the same set
    -- fails on this constraint instead of silently double-charging capacity.
    UNIQUE (booking_id, bucket_start)
);
CREATE INDEX idx_booking_capacity_holds_booking ON booking_capacity_holds (booking_id);
CREATE INDEX idx_booking_capacity_holds_rid_bucket ON booking_capacity_holds (restaurant_id, bucket_start) WHERE active;

-- +goose StatementBegin
CREATE FUNCTION apply_capacity_delta(p_restaurant uuid, p_bucket timestamptz, p_delta integer, p_limit integer)
    RETURNS void AS
$$
BEGIN
    IF p_delta = 0 THEN
        RETURN;
    END IF;
    IF p_delta > 0 THEN
        -- ON CONFLICT DO UPDATE is what makes this race-proof: a concurrent
        -- transaction blocks on the existing row's lock and, once the other
        -- side commits, re-applies its delta to the COMMITTED value — then the
        -- CHECK decides. No read-then-write window exists here.
        INSERT INTO restaurant_capacity_buckets AS b (restaurant_id, bucket_start, seats_taken, seats_limit)
        VALUES (p_restaurant, p_bucket, p_delta, p_limit)
        ON CONFLICT (restaurant_id, bucket_start)
            DO UPDATE SET seats_taken = b.seats_taken + p_delta,
                          seats_limit = p_limit;
    ELSE
        -- Release. seats_limit is intentionally untouched (see the table
        -- comment); the row is kept at zero rather than deleted, so the limit
        -- that applied to that bucket stays readable.
        UPDATE restaurant_capacity_buckets
        SET seats_taken = seats_taken + p_delta
        WHERE restaurant_id = p_restaurant
          AND bucket_start = p_bucket;
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION sync_capacity_bucket() RETURNS trigger AS
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
        -- Re-activation (e.g. waitlist → confirmed) is a real new claim on the
        -- capacity and can legitimately fail the CHECK if the venue filled up
        -- meanwhile. The status transition then aborts, which is the honest
        -- outcome.
        PERFORM apply_capacity_delta(NEW.restaurant_id, NEW.bucket_start, NEW.seats, NEW.seats_limit);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Row-level AFTER trigger: for a multi-row INSERT the rows fire in insertion
-- order, so inserting holds ordered by bucket_start gives every transaction the
-- same lock order on the bucket rows and rules out deadlocks between two
-- overlapping bookings. The repository guarantees that ordering.
CREATE TRIGGER trg_booking_capacity_holds_sync
    AFTER INSERT OR DELETE OR UPDATE OF active
    ON booking_capacity_holds
    FOR EACH ROW
EXECUTE FUNCTION sync_capacity_bucket();

-- +goose StatementBegin
CREATE FUNCTION sync_booking_capacity_holds_active() RETURNS trigger AS
$$
BEGIN
    -- The exact mirror of sync_booking_tables_active (0004) for capacity mode:
    -- a booking that no longer holds a seat must release it, and it must
    -- release it the same way whatever cancelled it.
    UPDATE booking_capacity_holds
    SET active = (NEW.status IN ('pending', 'confirmed', 'arrived'))
    WHERE booking_id = NEW.id
      AND active IS DISTINCT FROM (NEW.status IN ('pending', 'confirmed', 'arrived'));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_bookings_sync_capacity_holds_active
    AFTER UPDATE OF status
    ON bookings
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION sync_booking_capacity_holds_active();

RESET lock_timeout;

-- +goose Down
DROP TRIGGER trg_bookings_sync_capacity_holds_active ON bookings;
DROP FUNCTION sync_booking_capacity_holds_active();
DROP TRIGGER trg_booking_capacity_holds_sync ON booking_capacity_holds;
DROP FUNCTION sync_capacity_bucket();
DROP FUNCTION apply_capacity_delta(uuid, timestamptz, integer, integer);
DROP TABLE booking_capacity_holds;
DROP TABLE restaurant_capacity_buckets;
ALTER TABLE restaurants
    DROP CONSTRAINT chk_restaurants_capacity_seats_required,
    DROP CONSTRAINT chk_restaurants_capacity_seats,
    DROP CONSTRAINT chk_restaurants_capacity_mode;
ALTER TABLE restaurants
    DROP COLUMN booking_capacity_seats,
    DROP COLUMN booking_capacity_mode;
