-- +goose Up

-- Payments, wave 4 (docs/superpowers/specs/2026-07-21-payments-domain-design.md).
--
-- Money rules that hold everywhere below:
--   * every amount is a whole number of tiyn in a bigint (*_minor). No numeric,
--     no float — a rounding error here is a rounding error in someone's wallet;
--   * percentages are basis points in an integer (350 = 3.5%);
--   * currency is stored explicitly next to the amount it belongs to.

-- Registry of acquirers, driven from the admin panel. The provider CODE is the
-- primary key: it is a closed set validated in app code (domain.PaymentProvider)
-- and it is what restaurants.payment_provider and payments.provider reference by
-- value. Disabling a provider only blocks NEW payments — refunds for payments
-- already taken through it keep going through its adapter (spec §9.1).
--
-- Credentials are NOT here and never will be: keys live in env only (spec §8).
CREATE TABLE payment_providers
(
    provider   varchar PRIMARY KEY,
    is_enabled boolean     NOT NULL DEFAULT false,
    is_default boolean     NOT NULL DEFAULT false,
    priority   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- At most one default provider. A partial unique index on a constant expression
-- is the standard way to say "only one row may have this flag".
CREATE UNIQUE INDEX idx_payment_providers_default ON payment_providers ((true)) WHERE is_default;
CREATE INDEX idx_payment_providers_enabled ON payment_providers (priority) WHERE is_enabled;

-- Both providers are seeded DISABLED on purpose: switching an acquirer on is a
-- deliberate act in the admin panel, not a side effect of running a migration.
INSERT INTO payment_providers (provider, is_enabled, is_default, priority)
VALUES ('freedompay', false, false, 100),
       ('tiptoppay', false, false, 200);

-- Per-venue payment settings. All NULLABLE: NULL means "use the global default
-- from PAYMENTS_* env" (same convention as the booking policy columns).
ALTER TABLE restaurants
    ADD COLUMN payments_enabled          boolean,
    ADD COLUMN deposit_required          boolean,
    ADD COLUMN deposit_amount_minor      bigint CHECK (deposit_amount_minor >= 0),
    ADD COLUMN preorder_payment_required boolean,
    ADD COLUMN service_fee_bps           integer CHECK (service_fee_bps BETWEEN 0 AND 10000),
    ADD COLUMN payment_provider          varchar;

-- One row per attempt to pay. provider_payment_id is NULL until the acquirer
-- has answered the authorize call, so the uniqueness below is partial.
--
-- restaurant_id / user_id are denormalised out of the booking on purpose: a
-- payment is a financial record and must stay readable and reportable even if
-- the booking row is later deleted-by-cascade of something else.
CREATE TABLE payments
(
    id                  uuid PRIMARY KEY,
    booking_id          uuid        NOT NULL REFERENCES bookings (id) ON DELETE RESTRICT,
    restaurant_id       uuid        NOT NULL REFERENCES restaurants (id),
    user_id             uuid        REFERENCES users (id) ON DELETE SET NULL,
    provider            varchar     NOT NULL REFERENCES payment_providers (provider),
    provider_payment_id varchar,
    purpose             varchar     NOT NULL,
    status              varchar     NOT NULL DEFAULT 'created',
    amount_minor        bigint      NOT NULL CHECK (amount_minor >= 0),
    base_amount_minor   bigint      NOT NULL CHECK (base_amount_minor >= 0),
    fee_minor           bigint      NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    currency            varchar(3)  NOT NULL DEFAULT 'KZT',
    idempotency_key     varchar     NOT NULL,
    payment_url         varchar,
    authorized_at       timestamptz,
    captured_at         timestamptz,
    voided_at           timestamptz,
    failed_at           timestamptz,
    expires_at          timestamptz,
    failure_code        varchar,
    failure_message     varchar,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- The total the guest is charged is exactly base + service fee. Enforced
    -- here as well as in code: the server computes the amount (spec §8) and the
    -- database refuses to store a total that does not add up.
    CONSTRAINT chk_payments_amount_split CHECK (amount_minor = base_amount_minor + fee_minor)
);

-- Idempotency of our outgoing calls: one key per provider is one payment.
CREATE UNIQUE INDEX idx_payments_idempotency ON payments (provider, idempotency_key);
-- Identity on the acquirer's side; partial because the id arrives later.
CREATE UNIQUE INDEX idx_payments_provider_payment
    ON payments (provider, provider_payment_id)
    WHERE provider_payment_id IS NOT NULL;
-- One live payment per booking. 'created' is deliberately NOT live: a guest may
-- abandon a checkout and start a new one; money is only ever held once.
CREATE UNIQUE INDEX idx_payments_live_per_booking
    ON payments (booking_id)
    WHERE status IN ('authorized', 'captured');
CREATE INDEX idx_payments_booking ON payments (booking_id);
CREATE INDEX idx_payments_status_created ON payments (status, created_at);
CREATE INDEX idx_payments_restaurant_created ON payments (restaurant_id, created_at DESC);
CREATE INDEX idx_payments_user_created ON payments (user_id, created_at DESC);
-- Reconciliation: the worker scans holds that are about to lapse.
CREATE INDEX idx_payments_expires ON payments (expires_at) WHERE status = 'authorized';

-- Refunds are their own table because they are partial and repeatable: a
-- pre-order line may be refunded while the rest of the payment stands.
-- The "never refund more than what is left" rule is checked in the usecase
-- against SUM(amount_minor) of succeeded refunds; per-row it is at least
-- bounded to a positive amount here.
CREATE TABLE payment_refunds
(
    id                 uuid PRIMARY KEY,
    payment_id         uuid        NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    provider_refund_id varchar,
    amount_minor       bigint      NOT NULL CHECK (amount_minor > 0),
    currency           varchar(3)  NOT NULL DEFAULT 'KZT',
    status             varchar     NOT NULL DEFAULT 'created',
    reason             varchar,
    idempotency_key    varchar     NOT NULL,
    failure_code       varchar,
    failure_message    varchar,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_payment_refunds_payment ON payment_refunds (payment_id, created_at);
CREATE UNIQUE INDEX idx_payment_refunds_idempotency ON payment_refunds (payment_id, idempotency_key);
CREATE UNIQUE INDEX idx_payment_refunds_provider
    ON payment_refunds (payment_id, provider_refund_id)
    WHERE provider_refund_id IS NOT NULL;

-- Raw webhooks, stored exactly as received BEFORE any interpretation, including
-- the ones whose signature did not verify (signature_valid = false) — those are
-- evidence, not input. The unique index is the idempotency guard: an acquirer
-- redelivers, we process once.
--
-- payment_id is nullable and carries no FK by design: a webhook may reference a
-- provider_payment_id we do not know, and it must be recorded for investigation
-- rather than conjure a payment out of thin air (spec §7).
CREATE TABLE payment_events
(
    id                  uuid PRIMARY KEY,
    provider            varchar     NOT NULL,
    provider_event_id   varchar     NOT NULL,
    provider_payment_id varchar,
    payment_id          uuid,
    event_type          varchar,
    payload             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    signature_valid     boolean     NOT NULL DEFAULT false,
    received_at         timestamptz NOT NULL DEFAULT now(),
    processed_at        timestamptz,
    process_error       varchar
);
CREATE UNIQUE INDEX idx_payment_events_provider_event ON payment_events (provider, provider_event_id);
CREATE INDEX idx_payment_events_unprocessed ON payment_events (received_at) WHERE processed_at IS NULL;
CREATE INDEX idx_payment_events_payment ON payment_events (payment_id) WHERE payment_id IS NOT NULL;

-- Double-entry ledger. Every money movement of a payment is written as a pair
-- (or more) of entries whose debits equal their credits; the invariant is
-- asserted in domain.ValidateLedgerBalance and covered by a unit test.
-- Entries are append-only: a mistake is corrected by a reversing entry, never
-- by an UPDATE or a DELETE.
CREATE TABLE payment_ledger_entries
(
    id           uuid PRIMARY KEY,
    payment_id   uuid        NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    refund_id    uuid        REFERENCES payment_refunds (id) ON DELETE RESTRICT,
    account      varchar     NOT NULL,
    direction    varchar     NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_minor bigint      NOT NULL CHECK (amount_minor > 0),
    currency     varchar(3)  NOT NULL DEFAULT 'KZT',
    entry_type   varchar     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_payment_ledger_payment ON payment_ledger_entries (payment_id, created_at);
CREATE INDEX idx_payment_ledger_account ON payment_ledger_entries (account, created_at);

-- Transactional outbox, same shape and worker contract as booking_outbox:
-- written in the same transaction as the payment mutation it describes.
CREATE TABLE payment_outbox
(
    id           uuid PRIMARY KEY,
    payment_id   uuid        NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    event_type   varchar     NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX idx_payment_outbox_unpublished ON payment_outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX idx_payment_outbox_payment ON payment_outbox (payment_id);

-- +goose Down
DROP TABLE payment_outbox;
DROP TABLE payment_ledger_entries;
DROP TABLE payment_events;
DROP TABLE payment_refunds;
DROP TABLE payments;

ALTER TABLE restaurants
    DROP COLUMN payments_enabled,
    DROP COLUMN deposit_required,
    DROP COLUMN deposit_amount_minor,
    DROP COLUMN preorder_payment_required,
    DROP COLUMN service_fee_bps,
    DROP COLUMN payment_provider;

DROP TABLE payment_providers;
