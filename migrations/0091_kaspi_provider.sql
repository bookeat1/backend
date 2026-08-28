-- +goose Up

-- Registers Kaspi Pay as a fourth acquirer code and makes the venue↔acquirer
-- account mapping usable for it.
--
-- MIGRATION NUMBER: 0091 was taken with a deliberate gap over the highest
-- number live in ANY branch at the time of writing (0089 on
-- feat/notify-bot-migration, 0087 on develop / fix/venue-i18n-and-admin-read),
-- per the parallel-branch numbering rule — a colliding number cost five days
-- of production downtime once already.
--
-- Seeded DISABLED and non-default, the same convention as every other acquirer
-- (0007, 0011): turning one on is a deliberate admin act, never a side effect
-- of running a migration. Nothing here can start taking money by itself.
INSERT INTO payment_providers (provider, is_enabled, is_default, priority)
VALUES ('kaspi', false, false, 400);

-- WHERE A VENUE'S KASPI COMPANY IS RECORDED
--
-- It reuses restaurant_split_accounts (0077) rather than adding a column: that
-- table already answers exactly this question — "what is this venue's identity
-- at this acquirer" — with the right shape (one row per venue per provider,
-- deactivatable, never on the hot `restaurants` row). For Kaspi, account_ref
-- is the integer company id inside our multi-tenant Kaspi service
-- (kaspi.book-eat.com); the company's API KEY is NOT here and never will be —
-- keys live in env only (KASPI_COMPANY_API_KEYS).
--
-- WHY THE UNIQUE INDEX HAS TO CHANGE: 0077's index says one acquirer account
-- may be claimed by at most one active venue, which is right for a TipTop Pay
-- sub-merchant id (issued per venue). It is wrong for Kaspi: a company there
-- is a LEGAL ENTITY with a Kaspi cashier, and one entity legitimately owns
-- several venues that all settle into the same company. Enforcing 0077's rule
-- for Kaspi would mean refusing to onboard a restaurateur's second venue.
--
-- The rule is therefore kept in full for every other provider and lifted for
-- Kaspi alone, in one partial index instead of two, so there is still exactly
-- one place that decides this.
--
-- SAFE ON A POPULATED DATABASE: restaurant_split_accounts is empty in every
-- environment today (0077 shipped empty and no venue has been onboarded to
-- split payments), and even non-empty this only relaxes a constraint — no row
-- can become invalid. The DROP/CREATE pair takes a brief lock on a table with
-- no hot reads.
DROP INDEX IF EXISTS idx_restaurant_split_accounts_ref;
CREATE UNIQUE INDEX idx_restaurant_split_accounts_ref
    ON restaurant_split_accounts (provider, account_ref)
    WHERE is_active AND provider <> 'kaspi';

COMMENT ON INDEX idx_restaurant_split_accounts_ref IS
    'One acquirer account per active venue — except Kaspi, where one company (a legal entity) legitimately settles for several venues of the same owner.';

-- +goose Down

-- Kaspi mappings cannot outlive the provider row they reference (FK), and
-- restoring 0077's stricter index would fail on two venues sharing one Kaspi
-- company — which is a legitimate state this migration created. Both are
-- therefore removed together, in that order.
DELETE FROM restaurant_split_accounts WHERE provider = 'kaspi';

DROP INDEX IF EXISTS idx_restaurant_split_accounts_ref;
CREATE UNIQUE INDEX idx_restaurant_split_accounts_ref
    ON restaurant_split_accounts (provider, account_ref)
    WHERE is_active;

-- Guarded: a provider that already took money must not be deletable, and the
-- FK from payments would abort the whole rollback rather than say why.
DELETE FROM payment_providers
WHERE provider = 'kaspi'
  AND NOT EXISTS (SELECT 1 FROM payments WHERE provider = 'kaspi');
