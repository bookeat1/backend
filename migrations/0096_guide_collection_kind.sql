-- +goose Up

-- Articles split off from gastroguide collections — as a COLUMN, not a table.
--
-- Editorially these are two things: a COLLECTION is a curated set of venues
-- filed under rubrics ("лучшие завтраки"), an ARTICLE is a piece we wrote that
-- happens to point at venues ("что происходит на этой неделе"). Structurally
-- they are the same row. Every column this table has — slug, title, subtitle,
-- description, their *_i18n twins, cover, city, status, published_at, position
-- — means exactly the same thing for both, and both attach venues through
-- gastroguide_collection_venues with the same editorial order, note and
-- highlight. The only real difference is whether the item carries rubrics and
-- which screen it surfaces on.
--
-- A separate `articles` table would therefore have duplicated the venue-link
-- table, the repository, the usecase, the handler, the editor stack and the
-- admin screens — and, worse, it would have had to physically MOVE 4 live
-- published rows together with their venue links out of here, which is a
-- migration whose Down cannot be written without losing data. A `kind` column
-- leaves all 8 live rows and every venue link exactly where they are and makes
-- the Down a single DROP COLUMN.
--
-- Enumerated value as VARCHAR + CHECK, validated in Go
-- (internal/domain/gastroguide.go), never a Postgres ENUM — the standing rule
-- of this schema.

ALTER TABLE gastroguide_collections
    ADD COLUMN kind varchar NOT NULL DEFAULT 'collection'
        CHECK (kind IN ('collection', 'article'));

-- Backfill: the rubric IS the distinction. Everything already filed under a
-- rubric is a collection (the DEFAULT above has already said so); everything
-- with no rubric at all is an article. On the live database this splits the 8
-- published rows exactly 4/4 — kazakh-cuisine, mountains-gastronomy,
-- instagram-worthy and coffee-culture keep their rubrics and stay collections;
-- the four rubric-less pieces (chto-proishodit-na-etoi-nedele,
-- seichas-almaty-est-neveroyatno-horosho, gde-poest-s-rebenkom-v-almaty,
-- kafe-v-almaty-kuda-obyazatelno-nado-shodit-obzor-aqqu) become articles.
--
-- collection_id is NOT NULL in gastroguide_collection_categories, so NOT IN
-- cannot be poisoned by a NULL here.
UPDATE gastroguide_collections
SET kind = 'article'
WHERE id NOT IN (SELECT collection_id FROM gastroguide_collection_categories);

-- The guest listing now asks TWO different questions of the same table
-- ("published collections" and "published articles"), so kind leads the index:
-- it is the equality predicate, and position/id stay behind it as the ordered
-- part, which keeps the index serving the ORDER BY (position, id) inside one
-- kind without a sort.
--
-- It stays PARTIAL for the reason 0061 gave: the predicate drops every draft
-- and archived row, and the city filter is still left to the heap, because a
-- (city, …) key could not serve the "city IS NULL means everywhere" half of
-- the guest predicate anyway.
DROP INDEX idx_gastroguide_collections_live;

CREATE INDEX idx_gastroguide_collections_live
    ON gastroguide_collections (kind, position, id)
    WHERE status = 'published';

-- +goose Down

-- Lossless except for the kind flag itself: no row and no link is touched, the
-- index goes back to the exact definition migration 0061 created.
DROP INDEX idx_gastroguide_collections_live;

CREATE INDEX idx_gastroguide_collections_live
    ON gastroguide_collections (position, id)
    WHERE status = 'published';

ALTER TABLE gastroguide_collections
    DROP COLUMN kind;
