-- +goose Up

-- Promote the refund-policy columns (added and backfilled in 0047) to their
-- final shape: always present, with the owner-confirmed platform defaults
-- (refundable up to 24h before the event) and a non-negative window. Running
-- this only after 0047's backfill means no existing row can violate the NOT
-- NULL at the moment it is applied.
--
-- A negative window is nonsense (it would mean "refundable until AFTER the
-- event started"), so the CHECK refuses it at the DB level as well as in
-- usecase/events — a bad admin payload must never reach the table.
ALTER TABLE events
    ALTER COLUMN tickets_refundable SET DEFAULT true,
    ALTER COLUMN tickets_refundable SET NOT NULL,
    ALTER COLUMN ticket_refund_cutoff_minutes SET DEFAULT 1440,
    ALTER COLUMN ticket_refund_cutoff_minutes SET NOT NULL,
    ADD CONSTRAINT chk_events_ticket_refund_cutoff_nonneg
        CHECK (ticket_refund_cutoff_minutes >= 0);

-- The ticket snapshot is written by the purchase usecase from the event; the
-- DEFAULTs exist only so an INSERT that predates a redeploy (or a manual fix-up
-- row) cannot leave the column NULL and crash the scan. They mirror the event
-- defaults so a fallback row is not stricter than the platform promise.
ALTER TABLE event_tickets
    ALTER COLUMN refund_policy_refundable SET DEFAULT true,
    ALTER COLUMN refund_policy_refundable SET NOT NULL,
    ALTER COLUMN refund_policy_cutoff_minutes SET DEFAULT 1440,
    ALTER COLUMN refund_policy_cutoff_minutes SET NOT NULL,
    ADD CONSTRAINT chk_event_tickets_refund_cutoff_nonneg
        CHECK (refund_policy_cutoff_minutes >= 0);

-- +goose Down
ALTER TABLE event_tickets
    DROP CONSTRAINT chk_event_tickets_refund_cutoff_nonneg,
    ALTER COLUMN refund_policy_refundable DROP NOT NULL,
    ALTER COLUMN refund_policy_refundable DROP DEFAULT,
    ALTER COLUMN refund_policy_cutoff_minutes DROP NOT NULL,
    ALTER COLUMN refund_policy_cutoff_minutes DROP DEFAULT;

ALTER TABLE events
    DROP CONSTRAINT chk_events_ticket_refund_cutoff_nonneg,
    ALTER COLUMN tickets_refundable DROP NOT NULL,
    ALTER COLUMN tickets_refundable DROP DEFAULT,
    ALTER COLUMN ticket_refund_cutoff_minutes DROP NOT NULL,
    ALTER COLUMN ticket_refund_cutoff_minutes DROP DEFAULT;
