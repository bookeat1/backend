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
-- Owner decision (2026-07-25): the platform default is REFUNDABLE up to 24
-- hours before the event. A guest buying a ticket expects at least that much,
-- and a venue that wants a stricter rule sets it explicitly on its own event.
-- The backfill therefore opens refunds on existing events too. This is safe in
-- the only sense that matters — no money can leave for a purchase that was
-- never charged — because ticket sales run on the payments contour, which has
-- not been enabled in production (PAYMENTS_ENABLED=false); if that ever stops
-- being true, revisit this backfill before running it.
ALTER TABLE events
    ADD COLUMN tickets_refundable           boolean,
    ADD COLUMN ticket_refund_cutoff_minutes integer;

UPDATE events
SET tickets_refundable = true
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

-- Existing tickets take the same rules their event just got, so a ticket and
-- its event never disagree about what the buyer was promised.
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
