-- +goose Up

-- The Figma «Акции» card shows a promo PHOTO plus a «−30%» discount badge. The
-- photo already exists: migration 0060 added promos.cover_image_url (the FULL
-- public URL, same convention as menu_items.image_url / events.cover_image_url),
-- and it is already threaded through the promos vertical AND the main-screen
-- feed card. The ONLY thing the card still lacks is the number on the badge, so
-- this migration adds just that — a second "media" column would duplicate the
-- one 0060 already introduced.
--
-- discount_percent is the whole-percent price cut the badge renders ("−30%").
-- NULLABLE with no default on purpose: most promos are not a percentage-off
-- offer (a free dessert, a set menu, a happy hour) and inventing a 0 for them
-- would make the API claim "−0%". NULL means "no discount badge", the response
-- omits the field and the client draws no badge.
--
-- The CHECK bounds it to a sane percentage (0..100). 0 is allowed as an
-- explicit, honest value distinct from NULL, though the app is not expected to
-- badge a 0% cut. A value over 100 (a "−150%" offer) is a data error, refused at
-- the schema so no code path downstream has to defend against it.
--
-- Safe on live rows: a nullable column with no default is a catalog-only change,
-- Postgres does not rewrite the table, and the CHECK holds vacuously for every
-- existing NULL.
ALTER TABLE promos
    ADD COLUMN discount_percent integer
        CHECK (discount_percent IS NULL OR (discount_percent >= 0 AND discount_percent <= 100));

-- +goose Down

ALTER TABLE promos
    DROP COLUMN discount_percent;
