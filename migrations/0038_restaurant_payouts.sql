-- +goose Up

-- Restaurant payouts (выплаты заведениям), increment 1.
--
-- BookEat is the merchant of record: guest money lands in BookEat's acquirer
-- account and the venue's share is credited to the `restaurant` ledger account
-- at capture time (payment_ledger_entries). This module pays that owed balance
-- back out through an acquirer payout product (FreedomPay "выплаты"), tracking
-- what is owed and guaranteeing a captured credit is paid AT MOST ONCE.
--
-- Three tables:
--   restaurant_payout_destinations — where a venue's money goes. A provider
--       card TOKEN + a masked identifier only; NEVER a raw PAN (PCI). One live
--       destination per restaurant.
--   payouts                        — one settlement to one venue for a set of
--       ledger entries: amount (integer minor units), status machine
--       (pending→sent→paid|failed), our idempotency key, the acquirer ref.
--   payout_items                   — the claim table: each row ties one ledger
--       entry to the payout that pays it. UNIQUE(ledger_entry_id) is the single
--       arbiter that an entry is settled through at most one LIVE payout.
--
-- Safe on a populated DB: three brand-new tables, no change to existing rows.

CREATE TABLE restaurant_payout_destinations
(
    id                uuid PRIMARY KEY,
    restaurant_id     uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    provider          varchar     NOT NULL,
    -- 'freedompay_card_token' in increment 1. A raw PAN is never a valid value
    -- and there is deliberately no column that could hold one.
    method            varchar     NOT NULL,
    -- Provider-issued opaque handle for the payout instrument (FreedomPay card
    -- token, a UUID). The only address of the money; never a card number.
    token             varchar     NOT NULL,
    -- Merchant-side user id the token is registered under at the provider
    -- (FreedomPay pg_user_id). Paired with the token for a tokenized payout.
    -- Not a card number, not a secret. Empty until tokenization is wired.
    provider_customer_ref varchar NOT NULL DEFAULT '',
    -- Human-facing masked hint, display only (e.g. '440043******1234').
    masked_identifier varchar     NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_payout_destination_method
        CHECK (method IN ('freedompay_card_token')),
    -- One live destination per restaurant; a new one replaces the old in place.
    CONSTRAINT uq_payout_destinations_restaurant UNIQUE (restaurant_id)
);

CREATE TABLE payouts
(
    id                        uuid PRIMARY KEY,
    restaurant_id             uuid        NOT NULL REFERENCES restaurants (id),
    -- Money is always integer minor units, never a float.
    amount_minor              bigint      NOT NULL,
    currency                  varchar     NOT NULL,
    status                    varchar     NOT NULL,
    method                    varchar     NOT NULL,
    -- Snapshot of the destination token the payout was dispatched to, so a later
    -- change of the venue's destination does not rewrite history.
    destination_token         varchar     NOT NULL,
    -- Snapshot of the destination's provider user id (FreedomPay pg_user_id).
    destination_customer_ref  varchar     NOT NULL DEFAULT '',
    -- Acquirer-side payout id (pg_payment_id); NULL until the send is dispatched.
    provider_ref              varchar,
    -- Our own idempotency key, handed to the acquirer so a retried send resolves
    -- to the same provider payout instead of paying twice. Globally unique.
    idempotency_key           varchar     NOT NULL,
    failure_code              varchar,
    failure_reason            varchar,
    -- Reconciler lease clock + bookkeeping, same shape as payments (migration
    -- 0010): a payout stuck in `sent` is resolved by the payout reconciler.
    status_changed_at         timestamptz NOT NULL DEFAULT now(),
    reconcile_attempts        integer     NOT NULL DEFAULT 0,
    last_reconcile_attempt_at timestamptz,
    needs_manual_review       boolean     NOT NULL DEFAULT false,
    sent_at                   timestamptz,
    paid_at                   timestamptz,
    failed_at                 timestamptz,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_payouts_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT chk_payouts_status
        CHECK (status IN ('pending', 'sent', 'paid', 'failed'))
);

-- Idempotency: a send retry with the same key must find its own row, and two
-- concurrent generations must not both create a payout for the same key.
CREATE UNIQUE INDEX uq_payouts_idempotency ON payouts (idempotency_key);
-- Restaurant statement listing, newest first.
CREATE INDEX idx_payouts_restaurant ON payouts (restaurant_id, created_at DESC, id DESC);
-- Reconciler claim: stale payouts in a transient status, oldest first. Partial
-- index keeps only the actionable rows (a `paid`/`failed` payout is never
-- claimed), so the scan stays small as the table grows.
CREATE INDEX idx_payouts_reconcile ON payouts (status_changed_at)
    WHERE status = 'sent';

CREATE TABLE payout_items
(
    id                  uuid PRIMARY KEY,
    payout_id           uuid        NOT NULL REFERENCES payouts (id) ON DELETE CASCADE,
    ledger_entry_id     uuid        NOT NULL REFERENCES payment_ledger_entries (id),
    restaurant_id       uuid        NOT NULL REFERENCES restaurants (id),
    -- The entry's signed contribution to the payout: a restaurant CREDIT (owed)
    -- is positive, a restaurant DEBIT (refund/correction) negative. The sum over
    -- a payout's items equals payouts.amount_minor.
    amount_signed_minor bigint      NOT NULL,
    currency            varchar     NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    -- THE money-safety arbiter: a ledger entry is claimed by at most one live
    -- payout. A failed payout deletes its items (releasing them), so this stays
    -- literally true at all times and the money is never paid twice.
    CONSTRAINT uq_payout_items_ledger_entry UNIQUE (ledger_entry_id)
);

CREATE INDEX idx_payout_items_payout ON payout_items (payout_id);

-- +goose Down
DROP TABLE payout_items;
DROP TABLE payouts;
DROP TABLE restaurant_payout_destinations;
