-- +goose Up

-- "Выбор шефа" on the main screen is a rail of dishes from DIFFERENT venues.
-- Until now a dish could only be read one venue at a time (GET /restaurants/:id/menu)
-- and carried no signal that anybody had picked it, so the app rendered a
-- hard-coded placeholder list (apps/mobile/.../explore/placeholder.ts).
--
-- The flag lives on menu_items rather than in a separate curation table on
-- purpose: a picked dish is still just a dish, it is edited, hidden and deleted
-- through the same rows and the same tenant guard as any other menu item. A
-- side table would need its own cascade, its own ownership check and its own
-- way to go stale when the dish is removed.
--
-- NOT NULL DEFAULT false is safe on live rows: Postgres 11+ stores the default
-- in the catalog, so this does not rewrite the 2 310 existing rows.
ALTER TABLE menu_items
  ADD COLUMN is_featured boolean NOT NULL DEFAULT false;

-- The guest rail reads only featured AND available dishes, across venues, newest
-- first. A partial index keeps that read off a full scan of the whole menu
-- table while costing nothing for the 99% of rows that are not featured.
CREATE INDEX idx_menu_items_featured
  ON menu_items (updated_at DESC)
  WHERE is_featured AND is_available;

COMMENT ON COLUMN menu_items.is_featured IS
  'Dish is editorially picked for the cross-venue "chef''s picks" rail on the main screen. Set by venue staff or an admin; the guest rail also requires is_available.';

-- +goose Down

DROP INDEX IF EXISTS idx_menu_items_featured;
ALTER TABLE menu_items DROP COLUMN IF EXISTS is_featured;
