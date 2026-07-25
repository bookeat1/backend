-- +goose Up

-- Per-EVENT ticket refund policy (owner decision, 2026-07-25): a venue sets its
-- OWN rules per event instead of the platform imposing one. Two settings cover
-- what venues actually ask for:
--   tickets_refundable            -- may the guest get their money back at all
--   ticket_refund_cutoff_minutes  -- how many minutes before starts_at a refund
--                                    is still allowed (0 = right up to the start)
--
-- Added in two steps (0047 then 0048) so it is safe on a table that already has
-- live rows, exactly like 0034/0035: this migration adds the columns NULLABLE
-- and backfills every existing row, and 0048 promotes them to NOT NULL with a
-- DEFAULT and a CHECK. Each step stays trivially reversible and no in-flight
-- write is rejected between the two.
--
-- The backfilled default is TODAY'S behaviour, not a new right: until now
-- usecase/tickets had no policy at all and a guest could not self-refund. So
-- `false` grants nobody anything they did not have, and a venue opts in
-- explicitly. The cutoff default (1440 = 24h before the event) only starts to
-- matter once a venue turns refunds on.
ALTER TABLE events
    ADD COLUMN tickets_refundable           boolean,
    ADD COLUMN ticket_refund_cutoff_minutes integer;

UPDATE events
SET tickets_refundable = false
WHERE tickets_refundable IS NULL;

UPDATE events
SET ticket_refund_cutoff_minutes = 1440
WHERE ticket_refund_cutoff_minutes IS NULL;

-- Snapshot of the policy IN FORCE AT PURCHASE TIME, frozen onto the ticket the
-- same way unit_price_minor already is. The terms promise the guest that a
-- later change by the venue does not apply to a ticket already bought, and that
-- promise is only keepable if the row remembers the rules it was sold under —
-- reading the event's current columns at refund time would silently break it.
ALTER TABLE event_tickets
    ADD COLUMN refund_policy_refundable     boolean,
    ADD COLUMN refund_policy_cutoff_minutes integer;

-- Existing tickets predate the feature: they were sold under "no self-refund",
-- which is exactly what the event backfill above says, so copy it across.
UPDATE event_tickets t
SET refund_policy_refundable     = e.tickets_refundable,
    refund_policy_cutoff_minutes = e.ticket_refund_cutoff_minutes
FROM events e
WHERE e.id = t.event_id
  AND (t.refund_policy_refundable IS NULL OR t.refund_policy_cutoff_minutes IS NULL);

-- +goose Down
ALTER TABLE event_tickets
    DROP COLUMN refund_policy_refundable,
    DROP COLUMN refund_policy_cutoff_minutes;

ALTER TABLE events
    DROP COLUMN tickets_refundable,
    DROP COLUMN ticket_refund_cutoff_minutes;
