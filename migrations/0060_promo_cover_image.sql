-- +goose Up

-- A promo card had no picture at all: the main-screen rail (migration 0050)
-- projected NULL::varchar for every promo because the column did not exist, so
-- the app's promo block had nothing to render. Events already carry
-- cover_image_url (migration 0031) and the two are shown as the SAME card in
-- the same rail, so the column is named and typed identically — one convention,
-- not two.
--
-- NULLABLE on purpose, with no default: every promo that exists today has no
-- picture and inventing one (a placeholder URL, an empty string) would make the
-- API lie about it. NULL means "there is no image", the response omits the
-- field, and the client draws its own placeholder.
--
-- The value is the FULL public URL, exactly like restaurant_images.image_url,
-- menu_items.image_url and events.cover_image_url — today that is the
-- Cloudflare R2 managed domain (see ADR-013). Storing a bare key would be the
-- second convention this codebase has for the same thing.
--
-- Safe on live rows: a nullable column with no default is a catalog-only
-- change, Postgres does not rewrite the table.
ALTER TABLE promos
    ADD COLUMN cover_image_url varchar;

-- +goose Down

ALTER TABLE promos
    DROP COLUMN cover_image_url;
