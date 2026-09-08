-- +goose Up

-- In-app «Уведомления» feed for the GUEST (B5 part 2).
--
-- Everything the notification backbone shipped so far is EPHEMERAL: web push /
-- Telegram / mobile push fire once and leave no trace the guest can scroll
-- back to. The mobile app's «Уведомления» screen needs a DURABLE, per-user
-- history it can page through and mark read. This table is that store.
--
-- It is written by a NEW notifier (notifications.FeedNotifier) riding the SAME
-- dispatcher and booking outbox as the push channels — no second delivery
-- mechanism, no change to the bookings usecase. Idempotency is NOT the shared
-- notification_deliveries ledger here but the table's own unique key
-- (outbox_event_id, user_id): the dispatcher is at-least-once, so a redelivered
-- event must not append a duplicate feed row for the same guest.

CREATE TABLE notifications
(
    id              uuid PRIMARY KEY,
    -- The guest this entry is shown to. A booking made without an account
    -- (phone / admin-entered) produces no row — there is nobody to show it to,
    -- enforced in the notifier, not here.
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- "booking" | "reminder" | "promo" — the AppNotification.type the mobile
    -- client branches on. Free VARCHAR validated in app code, never a DB enum
    -- (same discipline as booking status / cancelled_by).
    type            varchar     NOT NULL,
    title           text        NOT NULL,
    body            text        NOT NULL,
    -- The originating booking / venue, kept for the app's deep-link. NULLABLE
    -- and ON DELETE SET NULL so a deleted booking or venue does not erase the
    -- guest's history entry — the feed is a durable log, not a live join.
    booking_id      uuid            NULL REFERENCES bookings (id) ON DELETE SET NULL,
    restaurant_id   uuid            NULL REFERENCES restaurants (id) ON DELETE SET NULL,
    -- The dedupe key's other half. The outbox event that produced this entry;
    -- a redelivery of the same event for the same user is a no-op INSERT.
    outbox_event_id uuid        NOT NULL REFERENCES booking_outbox (id) ON DELETE CASCADE,
    read_at         timestamptz     NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Idempotency: one feed entry per (outbox event, guest). The FeedNotifier's
    -- INSERT ... ON CONFLICT DO NOTHING leans on this under at-least-once
    -- dispatch.
    CONSTRAINT uq_notifications_event_user UNIQUE (outbox_event_id, user_id)
);

-- The feed query: a guest's own entries, newest first, keyset-paginated on
-- (created_at, id).
CREATE INDEX idx_notifications_user_created
    ON notifications (user_id, created_at DESC, id DESC);

-- The unread badge count: a partial index so COUNT(*) of unread stays cheap as
-- the read history grows without bound.
CREATE INDEX idx_notifications_user_unread
    ON notifications (user_id)
    WHERE read_at IS NULL;

-- +goose Down
DROP TABLE notifications;
