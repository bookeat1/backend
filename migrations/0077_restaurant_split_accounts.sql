-- +goose Up

-- WHERE A VENUE'S SUB-MERCHANT ACCOUNT LIVES (split payments, TipTop Pay).
--
-- A split payment divides ONE guest charge at the acquirer: the venue's share
-- goes straight to the venue's own sub-merchant account, the platform keeps its
-- commission. For that, every venue needs an acquirer-side identifier (TipTop
-- Pay: the sub-merchant Public ID from the merchant cabinet). Today NO venue
-- has one — they are issued one by one as venues are onboarded to acquiring —
-- so this table starts empty and stays empty until an operator fills it in.
--
-- WHY A TABLE AND NOT A COLUMN ON restaurant_payout_settings (the suggestion
-- this was weighed against):
--   * that table is one row per VENUE with no acquirer dimension, while this
--     identifier is one per (venue, acquirer): a venue can be onboarded at
--     TipTop Pay and not at FreedomPay, and the venue's own payment provider is
--     already overridable per venue (restaurants.payment_provider, 0007). A
--     single column could hold only one acquirer's answer and would silently
--     become the wrong one the day a second acquirer is switched on.
--   * restaurant_payout_settings answers "how often and above what threshold do
--     WE pay this venue out of money we already hold". This answers "where does
--     the ACQUIRER credit money we never hold". Storing them in one row makes
--     "which of these two addresses did this tenge take" a matter of reading
--     code instead of reading data.
--   * that table's rows are optional policy overrides written by a superadmin,
--     with updated_by audit. Reusing it would force a policy row into existence
--     for every venue merely to record an identifier.
--
-- WHY NOT A COLUMN ON restaurants: `restaurants` is the hottest read in the
-- product (feed, search, booking) and a money knob read once per checkout does
-- not belong in the row every guest-facing query pulls — the same reasoning
-- migration 0053 wrote down for restaurant_payout_settings.
--
-- SAFE ON A POPULATED DATABASE: a brand-new empty table, no lock on
-- `restaurants` beyond the FK's own share lock, no backfill, no default that
-- would invent an identifier nobody issued.

CREATE TABLE restaurant_split_accounts
(
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    -- The acquirer this identifier belongs to. FK to the acquirer registry
    -- seeded by 0007, so a typo cannot create a mapping for a provider that
    -- does not exist.
    provider      varchar     NOT NULL REFERENCES payment_providers (provider),
    -- The acquirer's own opaque handle for this venue (TipTop Pay sub-merchant
    -- Public ID). Not a secret in the acquirer-key sense — it identifies, it
    -- does not authorise — but it is the ADDRESS of the venue's money, so it is
    -- never written to a log in full. Non-empty by constraint: a blank string
    -- here would look configured and pay nobody.
    account_ref   text        NOT NULL,
    -- Lets a venue be suspended from split payments without deleting which
    -- account its historic payments were split to. An inactive row is treated
    -- exactly like a missing one by the checkout.
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- One identifier per venue per acquirer. This is the whole integrity story:
    -- two rows for the same pair would make "which account gets this venue's
    -- share" depend on row order.
    PRIMARY KEY (restaurant_id, provider),
    CONSTRAINT chk_restaurant_split_accounts_ref_not_blank CHECK (btrim(account_ref) <> '')
);

-- The same acquirer account must not be claimed by two venues: that is how one
-- venue silently collects another's money. Partial, because only ACTIVE rows
-- compete for an account — a deactivated mapping is history and may legitimately
-- be superseded by another venue's active one.
CREATE UNIQUE INDEX idx_restaurant_split_accounts_ref
    ON restaurant_split_accounts (provider, account_ref)
    WHERE is_active;

COMMENT ON TABLE restaurant_split_accounts IS
    'Venue identity at an acquirer for split payments (TipTop Pay sub-merchant Public ID). Empty until a venue is onboarded to acquiring; a venue without an ACTIVE row here cannot take a split payment at all.';

-- +goose Down

DROP INDEX IF EXISTS idx_restaurant_split_accounts_ref;
DROP TABLE IF EXISTS restaurant_split_accounts;
