-- +goose Up

-- preorder_min_amount_minor is the venue's OPTIONAL minimum pre-order total
-- (int64 MINOR units, tiyn). NULL = no minimum. When set and a booking's
-- computed pre-order total is > 0 but below this floor, attaching the pre-order
-- is rejected (usecase/preorder). It pairs with the existing
-- preorder_payment_required flag (migration 0007): required = the venue offers
-- pre-payment for pre-ordered dishes, min = the floor for such an order.
--
-- Nullable with a NULL-or-non-negative CHECK: safe to add to a live restaurants
-- table (no backfill, no rewrite, default NULL = current behaviour unchanged).
ALTER TABLE restaurants
    ADD COLUMN preorder_min_amount_minor bigint
        CHECK (preorder_min_amount_minor IS NULL OR preorder_min_amount_minor >= 0);

-- +goose Down
ALTER TABLE restaurants
    DROP COLUMN preorder_min_amount_minor;
