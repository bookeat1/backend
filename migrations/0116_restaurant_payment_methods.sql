-- +goose Up

-- Per-venue payment METHODS (owner decision 2026-09-24): instead of one
-- preferred acquirer (restaurants.payment_provider) a venue has two independent
-- switches:
--   kaspi — Kaspi Pay, needs a per-venue account in restaurant_split_accounts;
--   card  — every other acquirer, resolved to the platform's single enabled
--           non-Kaspi provider, no per-venue account.
-- The master switch restaurants.payments_enabled is unchanged and still gates
-- both. payment_provider is left in place (read only as a legacy hint).
--
-- Backwards compatible: existing rows behave as before. A venue that preferred
-- kaspi gets kaspi on / card off; every other venue keeps card on (the column
-- default) and kaspi off.
ALTER TABLE restaurants
    ADD COLUMN payment_kaspi_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN payment_card_enabled  boolean NOT NULL DEFAULT true;

UPDATE restaurants
   SET payment_kaspi_enabled = true,
       payment_card_enabled  = false
 WHERE payment_provider = 'kaspi';

-- +goose Down
ALTER TABLE restaurants
    DROP COLUMN payment_card_enabled,
    DROP COLUMN payment_kaspi_enabled;
