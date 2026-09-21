-- +goose Up
-- Correlates a menu_items row to a Kwaaka product for restaurants synced from
-- Kwaaka POS (restaurants.kwaaka_restaurant_id set, migration 0002). NULL for
-- every hand-entered dish — those are never touched by the sync worker.
ALTER TABLE menu_items
  ADD COLUMN kwaaka_product_id varchar;

-- The natural key an upsert matches on: one Kwaaka product maps to at most one
-- row per restaurant. Partial (WHERE kwaaka_product_id IS NOT NULL) so
-- hand-entered dishes (both NULL) never collide with each other.
CREATE UNIQUE INDEX uq_menu_items_kwaaka_product
  ON menu_items (restaurant_id, kwaaka_product_id)
  WHERE kwaaka_product_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS uq_menu_items_kwaaka_product;
ALTER TABLE menu_items DROP COLUMN IF EXISTS kwaaka_product_id;
