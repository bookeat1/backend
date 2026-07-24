-- +goose Up

-- Web-push notification backbone (increment 1). Three tables:
--
--   push_subscriptions              — a staff member's browser push endpoint,
--                                     registered by the frontend service worker.
--   restaurant_notification_settings — the per-restaurant channel toggle. A
--                                     MISSING row means "web push enabled"
--                                     (default on): disabling is an explicit
--                                     opt-out, so no backfill is needed here.
--   notification_deliveries         — the at-least-once dedupe ledger: one row
--                                     per (outbox event, channel, subscription)
--                                     that was successfully delivered, so a
--                                     redelivery of the same booking event never
--                                     re-notifies the same subscription.
--
-- The dispatcher drains booking_outbox and fans "booking.created" out to the
-- registered notifiers; web push is the first (and only) channel here.

CREATE TABLE push_subscriptions
(
    id            uuid PRIMARY KEY,
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    endpoint      text        NOT NULL,
    p256dh        text        NOT NULL,
    auth          text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- One browser subscription = one endpoint. Re-registering the same endpoint
    -- (e.g. after a key rotation) upserts in place rather than duplicating.
    CONSTRAINT uq_push_subscriptions_endpoint UNIQUE (endpoint)
);

-- Fan-out lookup: "all subscriptions of THIS booking's restaurant".
CREATE INDEX idx_push_subscriptions_restaurant ON push_subscriptions (restaurant_id);
-- Scoping: a staff member unregisters their own device(s).
CREATE INDEX idx_push_subscriptions_user ON push_subscriptions (user_id);

CREATE TABLE restaurant_notification_settings
(
    restaurant_id   uuid PRIMARY KEY REFERENCES restaurants (id) ON DELETE CASCADE,
    web_push_enabled boolean     NOT NULL DEFAULT true,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notification_deliveries
(
    id              uuid PRIMARY KEY,
    outbox_event_id uuid        NOT NULL REFERENCES booking_outbox (id) ON DELETE CASCADE,
    channel         text        NOT NULL,
    subscription_id uuid        NOT NULL REFERENCES push_subscriptions (id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- The dedupe key: a given outbox event is delivered to a given subscription
    -- over a given channel AT MOST ONCE. The unique index also serves the
    -- "already delivered?" pre-check the dispatcher runs before each send.
    CONSTRAINT uq_notification_deliveries UNIQUE (outbox_event_id, channel, subscription_id)
);

-- +goose Down
DROP TABLE notification_deliveries;
DROP TABLE restaurant_notification_settings;
DROP TABLE push_subscriptions;
