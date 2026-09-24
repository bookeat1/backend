-- +goose Up

-- Pre-order hold + booking gate (owner decisions 2026-09-24).
--
-- bookings.released_to_venue_at: NULL = the booking is HIDDEN from the venue
-- (it carries a pre-order whose payment has not been authorized yet). Set when
-- the pre-order payment goes created -> authorized (or, for every booking that
-- never had a gate, at creation). Existing rows are backfilled with created_at,
-- so nothing already visible disappears.
ALTER TABLE bookings ADD COLUMN released_to_venue_at timestamptz NULL;
UPDATE bookings SET released_to_venue_at = created_at;
CREATE INDEX idx_bookings_unreleased ON bookings (created_at)
    WHERE released_to_venue_at IS NULL AND status = 'pending';

-- payments.requires_confirmation: HOW the payment was sent to the acquirer
-- (true = two-stage hold that must be confirmed/voided, false = one-stage
-- charge). The webhook decides by THIS column, not by purpose, so a link issued
-- as one-stage before this rollout and paid after it is still handled as
-- one-stage. NOT NULL DEFAULT true is a metadata-only change on PG11+.
ALTER TABLE payments ADD COLUMN requires_confirmation boolean NOT NULL DEFAULT true;
-- Before this change TipTopPay pre-orders/tickets and Kaspi pre-orders were
-- one-stage; everything else was a hold.
UPDATE payments SET requires_confirmation = false
 WHERE (provider = 'tiptoppay' AND purpose IN ('preorder', 'ticket'))
    OR (provider = 'kaspi' AND purpose IN ('preorder', 'ticket'));

-- +goose Down
ALTER TABLE payments DROP COLUMN requires_confirmation;
DROP INDEX idx_bookings_unreleased;
ALTER TABLE bookings DROP COLUMN released_to_venue_at;
