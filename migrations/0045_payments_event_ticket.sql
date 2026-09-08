-- +goose Up

-- Reuse the payments contour for event-ticket purchases WITHOUT forking it: a
-- ticket purchase creates a real `payments` row and is confirmed by the exact
-- same webhook/capture flow bookings use. A payment therefore becomes
-- POLYMORPHIC over its subject — a booking XOR an event ticket.
--
-- Safety on a populated payments table:
--   * booking_id drops NOT NULL, but every existing row keeps its value, so the
--     XOR check below is satisfied for all of them (booking_id set,
--     event_ticket_id NULL → exactly one non-null);
--   * event_ticket_id is a new nullable column → no rewrite, no backfill.
ALTER TABLE payments
    ALTER COLUMN booking_id DROP NOT NULL;

ALTER TABLE payments
    ADD COLUMN event_ticket_id uuid REFERENCES event_tickets (id) ON DELETE RESTRICT;

-- Exactly one subject: a payment pays for a booking OR for an event ticket,
-- never both, never neither. num_nonnulls is a Postgres built-in.
ALTER TABLE payments
    ADD CONSTRAINT chk_payments_subject
        CHECK (num_nonnulls(booking_id, event_ticket_id) = 1);

-- One live payment per ticket, mirroring idx_payments_live_per_booking (money is
-- held at most once per ticket). NULL event_ticket_id (every booking payment) is
-- excluded, so booking payments never collide here.
CREATE UNIQUE INDEX idx_payments_live_per_ticket
    ON payments (event_ticket_id)
    WHERE event_ticket_id IS NOT NULL
      AND status IN ('authorized', 'capturing', 'voiding', 'captured');
CREATE INDEX idx_payments_event_ticket ON payments (event_ticket_id)
    WHERE event_ticket_id IS NOT NULL;

-- +goose Down

-- Down restores the booking-only shape. It assumes no ticket-only payments
-- exist (booking_id NULL rows) — true before this feature ships and in the
-- migration round-trip test, which runs on booking rows only. A real rollback
-- with live ticket payments would need those settled/removed first.
DROP INDEX idx_payments_event_ticket;
DROP INDEX idx_payments_live_per_ticket;
ALTER TABLE payments DROP CONSTRAINT chk_payments_subject;
ALTER TABLE payments DROP COLUMN event_ticket_id;
ALTER TABLE payments ALTER COLUMN booking_id SET NOT NULL;
