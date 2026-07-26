-- +goose Up

-- LOCK SAFETY, same reasoning as 0058: CREATE CONSTRAINT TRIGGER takes
-- SHARE ROW EXCLUSIVE on the table, which conflicts with every writer of it, so
-- it must not be allowed to queue behind a long transaction and stall traffic
-- behind itself. With lock_timeout the deploy fails loudly after 3s and is
-- retried at a quieter moment instead. RESET so the timeout does not leak into
-- the migrations that follow in the same session.
SET lock_timeout = '3s';

-- `active` DERIVED AGAIN AT COMMIT TIME — the half that 0057 / 0058 could not
-- cover (review finding, 25.07.2026: the SEVENTH hole in this seam, reproduced
-- on a real database in
-- internal/usecase/bookings/venue_policy_midloop_cancel_integration_test.go).
--
-- THE HOLE. Occupancy rows (booking_capacity_holds, booking_tables) are written
-- by transactions that live for a while: a capacity-mode switch walks up to
-- domain.MaxReconcileBookings bookings, an amendment re-writes one booking's
-- holds and links. Their protection had two halves that do not compose under
-- READ COMMITTED:
--
--   * 0057 / 0058 (BEFORE INSERT) derive `active` from the booking status
--     committed AT THE MOMENT OF THE INSERT. A cancel that commits LATER — but
--     still before the writer commits — is invisible to them.
--   * 0054 / 0004 (AFTER UPDATE OF status) flip the rows of the booking whose
--     status changed. They cannot flip a row the other transaction has not
--     committed yet: it does not exist for them.
--
-- Nothing serialises the two. The FK from the occupancy row takes FOR KEY SHARE
-- on the booking, `UPDATE bookings SET status` takes FOR NO KEY UPDATE — those
-- are compatible, so the cancel never waits, and status.go deliberately takes no
-- venue lock (it is the hottest path on the platform). Observed result, both
-- directions: a cancelled booking left holding 10 ACTIVE capacity holds with
-- seats_taken = 4 across its window, or an ACTIVE booking_tables link that makes
-- the GiST exclusion constraint refuse a real party for that table. Permanent —
-- every reconciliation walks LIVE bookings only, so a cancelled booking's rows
-- are never revisited.
--
-- THE FIX. Derive `active` a second time, per inserted row, from a DEFERRED
-- CONSTRAINT TRIGGER — i.e. at the writer's COMMIT, after everything it wanted
-- to do has been done. Two properties make this the right place:
--
--   1. the trigger body is a new statement, so under READ COMMITTED it takes a
--      FRESH snapshot and sees every status change committed since the INSERT.
--      That closes the whole loop: a cancel committed at ANY point during the
--      writer's transaction is seen, whether it landed before that booking's
--      rows were written, between the DELETE and the INSERT of
--      ReplaceForBooking, or after them.
--   2. the row's own writer is the one that cleans up, at a moment when no
--      application code runs between the check and the commit — so the window
--      the check leaves open is closed by a lock nobody has to wait on for long
--      (point 3 below), instead of by making every cancel on the platform wait
--      for whatever mode switch happens to be running.
--
--   3. …and the lock. A plain read would still leave a race: a status UPDATE
--      that is IN FLIGHT (uncommitted) while this trigger reads is invisible to
--      it, and its own flip already missed our uncommitted rows — so the phantom
--      would survive if that cancel commits between this check and our commit.
--      The read therefore takes FOR NO KEY UPDATE, which conflicts with the
--      FOR NO KEY UPDATE any `UPDATE bookings` holds:
--
--        * lock free  → we hold it until commit, so a cancel arriving after this
--          point WAITS for our commit and its own AFTER UPDATE trigger (0054 /
--          0004) then sees our rows, committed, and flips them. Correct, and the
--          wait lasts only for the remainder of our commit.
--        * lock taken → NOWAIT, so we never wait, and therefore can never be
--          part of a deadlock cycle (the deadlock risk of locking bookings
--          EARLY, before touching the child rows, is real: the cancel path takes
--          the booking first and its child rows second, so a writer that
--          reversed that order could deadlock with it). We raise
--          serialization_failure instead: the writer rolls back, no phantom is
--          committed, and the caller retries. sqltx.Manager maps that to
--          domain.ErrAlreadyExists → 409, not a 500.
--
-- The alternatives, for the record: locking the booking set at the start of the
-- switch makes every cancel of that venue wait for the whole switch (up to the
-- 15s write timeout) and was measured to stall the reproduction tests outright;
-- taking the venue advisory lock in status.go puts the platform's hottest write
-- path behind an admin action; re-reading the status from Go after each write
-- leaves exactly the race point 3 closes and puts `active` back into
-- application code, which 0004's rule forbids.
--
-- COST. One indexed SELECT (plus a row lock already held by anybody who touched
-- the booking in the same transaction) per inserted occupancy row, at commit.
-- The create path locks its own freshly inserted booking row, so it can neither
-- wait nor fail here.
--
-- SAFE ON A POPULATED DATABASE: two functions and two triggers, no row is read
-- or rewritten by the migration itself.

-- +goose StatementBegin
CREATE FUNCTION booking_capacity_hold_active_at_commit() RETURNS trigger AS
$$
DECLARE
    v_live boolean;
BEGIN
    IF NOT NEW.active THEN
        -- Landed inactive (waitlisted or already-cancelled booking): it occupies
        -- nothing, and nothing here may ever hand occupancy BACK — re-claiming a
        -- seat is a status transition, judged by 0054 / 0057 under today's
        -- capacity.
        RETURN NULL;
    END IF;
    BEGIN
        SELECT (b.status IN ('pending', 'confirmed', 'arrived'))
        INTO v_live
        FROM bookings b
        WHERE b.id = NEW.booking_id
            FOR NO KEY UPDATE NOWAIT;
    EXCEPTION
        WHEN lock_not_available THEN
            RAISE EXCEPTION
                'booking % is being changed by another transaction; its capacity holds cannot be committed safely — retry',
                NEW.booking_id
                USING ERRCODE = 'serialization_failure';
    END;
    -- IS NOT TRUE, not NOT: a NULL would mean the booking row vanished, which
    -- the NOT NULL FK makes impossible; treating it as "not live" keeps the
    -- failure mode on the safe side (a hold that takes nothing).
    IF v_live IS NOT TRUE THEN
        UPDATE booking_capacity_holds
        SET active = false
        WHERE id = NEW.id
          AND active;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_booking_capacity_holds_active_at_commit
    AFTER INSERT
    ON booking_capacity_holds
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
EXECUTE FUNCTION booking_capacity_hold_active_at_commit();

-- +goose StatementBegin
CREATE FUNCTION booking_table_active_at_commit() RETURNS trigger AS
$$
DECLARE
    v_live boolean;
BEGIN
    IF NEW.booking_id IS NULL THEN
        -- External hold (0030): no booking owns it, so there is no status to
        -- derive from — exactly as in 0058.
        RETURN NULL;
    END IF;
    IF NOT NEW.active THEN
        RETURN NULL;
    END IF;
    BEGIN
        SELECT (b.status IN ('pending', 'confirmed', 'arrived'))
        INTO v_live
        FROM bookings b
        WHERE b.id = NEW.booking_id
            FOR NO KEY UPDATE NOWAIT;
    EXCEPTION
        WHEN lock_not_available THEN
            RAISE EXCEPTION
                'booking % is being changed by another transaction; its table placement cannot be committed safely — retry',
                NEW.booking_id
                USING ERRCODE = 'serialization_failure';
    END;
    IF v_live IS NOT TRUE THEN
        UPDATE booking_tables
        SET active = false
        WHERE id = NEW.id
          AND active;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_booking_tables_active_at_commit
    AFTER INSERT
    ON booking_tables
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
EXECUTE FUNCTION booking_table_active_at_commit();

COMMENT ON COLUMN booking_capacity_holds.active IS
    'Whether this hold currently counts against the venue capacity. Never written by application code: derived from the booking status on INSERT (0057), re-derived from it at the writer''s COMMIT (0059), and flipped by the status trigger (0054).';

COMMENT ON COLUMN booking_tables.active IS
    'Whether this link currently occupies the table (and is therefore seen by the GiST exclusion constraint). Never written by application code: derived from the booking status on INSERT (0058), re-derived from it at the writer''s COMMIT (0059), and flipped by the status trigger (0004). Rows owned by an external reservation keep the value their own lifecycle sets.';

RESET lock_timeout;

-- +goose Down
DROP TRIGGER trg_booking_tables_active_at_commit ON booking_tables;
DROP FUNCTION booking_table_active_at_commit();
DROP TRIGGER trg_booking_capacity_holds_active_at_commit ON booking_capacity_holds;
DROP FUNCTION booking_capacity_hold_active_at_commit();

-- Restore the column comments 0057 / 0058 left.
COMMENT ON COLUMN booking_capacity_holds.active IS
    'Whether this hold currently counts against the venue capacity. Never written by application code: set from the booking status on INSERT (0057) and flipped by the status trigger (0054).';

COMMENT ON COLUMN booking_tables.active IS
    'Whether this link currently occupies the table (and is therefore seen by the GiST exclusion constraint). Never written by application code: derived from the booking status on INSERT (0058) and flipped by the status trigger (0004). Rows owned by an external reservation keep the value their own lifecycle sets.';
