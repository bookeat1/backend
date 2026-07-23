-- +goose Up

-- Registers Partners Pay (partnerspay.co) as a third acquirer code. Seeded
-- DISABLED and non-default, same convention as freedompay/tiptoppay in
-- 0007_payments.sql: turning an acquirer on is a deliberate admin-panel act,
-- never a side effect of a migration.
--
-- There is no working adapter behind this code yet — see
-- internal/infrastructure/payment/partnerspay for the template implementation
-- and docs/payments/partnerspay-integration-questions.md for what is missing
-- before it can take real traffic. The row exists now so the registry
-- (payment_providers) and the domain closed set (domain.PaymentProvider)
-- agree, and so the admin panel can show the provider as "known, not yet
-- usable" instead of "unknown code" once someone starts wiring the UI.
INSERT INTO payment_providers (provider, is_enabled, is_default, priority)
VALUES ('partnerspay', false, false, 300);

-- +goose Down
DELETE FROM payment_providers WHERE provider = 'partnerspay';
