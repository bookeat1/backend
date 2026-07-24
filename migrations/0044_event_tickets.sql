-- +goose Up

-- Event ticketing (roadmap #2): a guest buys N tickets for a ticketed event.
-- The event already carries ticketed / ticket_price_minor / capacity (migration
-- 0031, fields only); THIS table is the ticket-as-object + the capacity ledger.
--
-- Money rule (as everywhere): integer minor units in a bigint, never a float.
-- total_minor is enforced to equal unit_price_minor * quantity at the DB level,
-- so a client-supplied total can never disagree with what the server priced.
--
-- restaurant_id is denormalised out of the event on purpose (an event's venue
-- never changes): the admin "tickets sold" listing, the revenue aggregate and
-- the RBAC tenant-scoping all read it without a join, exactly like payments
-- denormalises restaurant_id out of the booking.
--
-- status is a VARCHAR validated in Go (internal/domain/event_ticket.go), never a
-- Postgres ENUM, same convention as every other table here:
--   pending   -- reserved, awaiting payment (holds capacity)
--   paid      -- payment captured (holds capacity)
--   cancelled -- payment failed/expired/voided or reservation released (frees capacity)
--   refunded  -- a paid ticket was refunded (frees capacity)
-- Capacity is HELD by pending+paid rows only; cancelled/refunded free the seats.
CREATE TABLE event_tickets
(
    id                        uuid PRIMARY KEY,
    event_id                  uuid        NOT NULL REFERENCES events (id) ON DELETE RESTRICT,
    restaurant_id             uuid        NOT NULL REFERENCES restaurants (id),
    -- NULL = a guest checkout without an account (same as payments.user_id).
    user_id                   uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- Contact details for a guest buyer (and a convenience copy for an account
    -- buyer). guest_phone is stored normalised (internal/auth/phone.Normalize).
    guest_name                varchar     NOT NULL DEFAULT '',
    guest_phone               varchar     NOT NULL DEFAULT '',
    guest_email               varchar     NOT NULL DEFAULT '',
    quantity                  integer     NOT NULL CHECK (quantity > 0),
    unit_price_minor          bigint      NOT NULL CHECK (unit_price_minor >= 0),
    total_minor               bigint      NOT NULL CHECK (total_minor >= 0),
    currency                  varchar(3)  NOT NULL DEFAULT 'KZT',
    status                    varchar     NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending', 'paid', 'cancelled', 'refunded')),
    -- payment_id is a plain uuid, NOT a foreign key by design — payments
    -- references event_tickets (migration 0045), and a mutual FK would be a
    -- cycle. Same FK-less back-reference convention as payment_events.payment_id.
    -- NULL until the payment intent is created in the purchase flow.
    payment_id                uuid,
    -- The buyer's retry token, server-scoped to the event. A lost response and a
    -- client retry with the same key resolve to the SAME ticket instead of
    -- reserving a second batch of seats — the idempotent-create pattern used for
    -- payments and favorites.
    purchase_idempotency_key  varchar     NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    -- The server prices every ticket (quantity * unit); the DB refuses a total
    -- that does not add up, so a tampered client total can never be stored.
    CONSTRAINT chk_event_tickets_total CHECK (total_minor = unit_price_minor * quantity)
);

-- One ticket per (event, idempotency key): the reservation is idempotent.
CREATE UNIQUE INDEX idx_event_tickets_idempotency
    ON event_tickets (event_id, purchase_idempotency_key);
-- Capacity SUM (WHERE status IN ('pending','paid')) + admin per-status counts.
CREATE INDEX idx_event_tickets_event_status ON event_tickets (event_id, status);
-- Admin "tickets sold for this venue", newest first.
CREATE INDEX idx_event_tickets_restaurant ON event_tickets (restaurant_id, created_at DESC);
-- A guest's own tickets.
CREATE INDEX idx_event_tickets_user ON event_tickets (user_id, created_at DESC)
    WHERE user_id IS NOT NULL;

-- +goose Down
DROP TABLE event_tickets;
