-- +goose Up
-- Kwaaka phase 2: pre-order goes to the venue's POS as a table order.
-- Only adds tables; nothing existing is touched. Enumerations are VARCHAR
-- validated in domain (no CREATE TYPE ... AS ENUM).

-- Per-venue sending settings, superadmin-managed (ADR-049).
CREATE TABLE restaurant_kwaaka_order_settings
(
    restaurant_id        uuid PRIMARY KEY REFERENCES restaurants (id) ON DELETE CASCADE,
    orders_enabled       boolean     NOT NULL DEFAULT false,
    -- which Kwaaka linkage the pool was chosen for; a changed
    -- restaurants.kwaaka_restaurant_id makes the pool stale
    kwaaka_restaurant_id varchar     NOT NULL,
    lead_minutes         integer CHECK (lead_minutes BETWEEN 0 AND 720), -- NULL = platform default
    enabled_at           timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    updated_by           uuid -- no FK on users (tests TRUNCATE users CASCADE)
);

-- The pool of POS tables the venue's pre-orders are spread over.
CREATE TABLE restaurant_kwaaka_pool_tables
(
    restaurant_id   uuid     NOT NULL REFERENCES restaurant_kwaaka_order_settings (restaurant_id) ON DELETE CASCADE,
    kwaaka_table_id varchar  NOT NULL,
    position        smallint NOT NULL,
    label           varchar  NOT NULL DEFAULT '',
    PRIMARY KEY (restaurant_id, kwaaka_table_id)
);

-- One row per booking; id is the order_id sent to Kwaaka (idempotency key).
CREATE TABLE kwaaka_kitchen_orders
(
    id                   uuid PRIMARY KEY,
    booking_id           uuid        NOT NULL UNIQUE REFERENCES bookings (id) ON DELETE CASCADE,
    restaurant_id        uuid        NOT NULL,
    kwaaka_restaurant_id varchar     NOT NULL,
    kwaaka_table_id      varchar     NOT NULL,
    table_shared         boolean     NOT NULL DEFAULT false,
    kwaaka_order_id      varchar,
    status               varchar     NOT NULL,
    trigger              varchar     NOT NULL,
    paid                 boolean     NOT NULL,
    paid_amount_minor    bigint,
    total_minor          bigint      NOT NULL,
    partial              boolean     NOT NULL DEFAULT false,
    request_snapshot     jsonb       NOT NULL,
    booking_starts_at    timestamptz NOT NULL,
    deadline_at          timestamptz NOT NULL,
    table_hold_until     timestamptz NOT NULL,
    table_released_at    timestamptz,
    attempts             integer     NOT NULL DEFAULT 0,
    outcome_unknown      boolean     NOT NULL DEFAULT false,
    next_attempt_at      timestamptz,
    last_error           text,
    error_code           varchar,
    sent_at              timestamptz,
    cancel_requested_at  timestamptz,
    cancelled_at         timestamptz,
    cancel_reason        varchar,
    pos_status_raw       varchar,
    pos_state            varchar,
    pos_status_at        timestamptz,
    pos_status_source    varchar,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_kwaaka_orders_due ON kwaaka_kitchen_orders (next_attempt_at)
    WHERE status IN ('sending', 'cancelling');
CREATE INDEX idx_kwaaka_orders_table ON kwaaka_kitchen_orders (restaurant_id, kwaaka_table_id)
    WHERE status IN ('sending', 'sent', 'cancelling', 'failed_unknown') AND table_released_at IS NULL;
CREATE INDEX idx_kwaaka_orders_pos_id ON kwaaka_kitchen_orders (kwaaka_order_id)
    WHERE kwaaka_order_id IS NOT NULL;
CREATE INDEX idx_kwaaka_orders_poll ON kwaaka_kitchen_orders (pos_status_at)
    WHERE status IN ('sent', 'cancelling');

-- Inbox of Kwaaka webhooks (ADR-050): fast 200, parse in the background.
CREATE TABLE kwaaka_webhook_events
(
    id               uuid PRIMARY KEY,
    kind             varchar     NOT NULL, -- order_status | reserve_status
    dedup_key        varchar     NOT NULL UNIQUE, -- sha256(body), hex
    body             bytea       NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT now(),
    attempts         integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz,
    processed_at     timestamptz,
    outcome          varchar,
    process_error    text,
    kitchen_order_id uuid
);
CREATE INDEX idx_kwaaka_webhook_pending ON kwaaka_webhook_events (next_attempt_at)
    WHERE processed_at IS NULL;
CREATE INDEX idx_kwaaka_webhook_done ON kwaaka_webhook_events (processed_at)
    WHERE processed_at IS NOT NULL;

-- +goose Down
-- IRREVERSIBLE BY DATA: after the first real send do not run this in prod
-- (it erases the order history); roll back with KWAAKA_ORDERS_ENABLED=false.
DROP TABLE IF EXISTS kwaaka_webhook_events;
DROP TABLE IF EXISTS kwaaka_kitchen_orders;
DROP TABLE IF EXISTS restaurant_kwaaka_pool_tables;
DROP TABLE IF EXISTS restaurant_kwaaka_order_settings;
