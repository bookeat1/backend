-- +goose Up

-- Gastroguide — editorial collections of venues ("где поесть с детьми",
-- "лучшие завтраки"). Unlike the merchandising feed (migration 0050), the
-- content here is written by US, not submitted by a venue: there is no
-- moderation axis and no paid placement, only an editor's decision about what
-- goes in, in which order, and whether it is live yet.
--
-- Four tables, and why exactly these four:
--   gastroguide_categories             — the guide's rubrics
--   gastroguide_collections            — the collections themselves
--   gastroguide_collection_categories  — collection ↔ rubric (many-to-many)
--   gastroguide_collection_venues      — collection ↔ venue, with the order
--
-- ORDER IS DATA, not a side effect of the plan. Every list a guest sees is
-- ordered by an explicit integer the editor sets, with the row's own id as the
-- final tie-break, so two rows sharing a number still come back in the same
-- sequence on every request.
--
-- Localization reuses the catalog's mechanism unchanged: a base ru column plus
-- an optional *_i18n jsonb, resolved by domain.I18n.Resolve. No new i18n
-- machinery.
--
-- Enumerated values are VARCHAR validated in Go (internal/domain/gastroguide.go),
-- never a Postgres ENUM.

CREATE TABLE gastroguide_categories
(
    id         uuid PRIMARY KEY,
    -- slug is the client-facing stable name ("breakfasts"): the app links to a
    -- rubric by slug, so it must survive a title rewrite.
    slug       varchar     NOT NULL UNIQUE,
    title      varchar     NOT NULL,
    title_i18n jsonb,
    position   integer     NOT NULL DEFAULT 0,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_gastroguide_categories_order
    ON gastroguide_categories (position, id)
    WHERE is_active;

CREATE TABLE gastroguide_collections
(
    id               uuid PRIMARY KEY,
    slug             varchar     NOT NULL UNIQUE,
    title            varchar     NOT NULL,
    title_i18n       jsonb,
    subtitle         varchar     NOT NULL DEFAULT '',
    subtitle_i18n    jsonb,
    description      text        NOT NULL DEFAULT '',
    description_i18n jsonb,
    -- Full public URL, same convention as promos/events/venue images.
    -- NULL = no cover; the API omits it instead of inventing a placeholder.
    cover_image_url  varchar,
    -- NULL = the collection is shown in every city ("лучшие завтраки" as a
    -- concept). A city value scopes it to one city's home screen. Not a FK:
    -- there is no cities table — the catalog stores the city as VARCHAR on
    -- restaurants and the known values live in domain.City.
    city             varchar,
    -- Publication axis, so an editor can stage a collection before it goes
    -- live: draft (invisible), published (visible from published_at on),
    -- archived (was live, withdrawn — kept for its links, invisible).
    status           varchar     NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'published', 'archived')),
    -- When the collection becomes visible. Required while published, which
    -- makes the guest rule one comparison with no NULL branch AND gives
    -- scheduled publication for free (set it in the future and the collection
    -- appears by itself).
    published_at     timestamptz,
    position         integer     NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gastroguide_collections_published_needs_time
        CHECK (status <> 'published' OR published_at IS NOT NULL)
);

-- The guest listing reads published collections in editorial order. One partial
-- index is enough: the predicate keeps every draft/archived row out, and the
-- city filter is left to the heap — collections are counted in tens, and a
-- (city, position) index could not serve the "city IS NULL means everywhere"
-- half of the predicate anyway.
CREATE INDEX idx_gastroguide_collections_live
    ON gastroguide_collections (position, id)
    WHERE status = 'published';

-- A collection belongs to any number of rubrics, and a rubric holds any number
-- of collections: "лучшие завтраки" is honestly both "Завтраки" and "Утро", and
-- forcing one category_id onto the collection would make an editor duplicate
-- the collection to say so. position orders the collections INSIDE the rubric.
CREATE TABLE gastroguide_collection_categories
(
    collection_id uuid        NOT NULL REFERENCES gastroguide_collections (id) ON DELETE CASCADE,
    category_id   uuid        NOT NULL REFERENCES gastroguide_categories (id) ON DELETE CASCADE,
    position      integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, category_id)
);

CREATE INDEX idx_gastroguide_collection_categories_order
    ON gastroguide_collection_categories (category_id, position, collection_id);

-- Collection ↔ venue. The primary key is (collection_id, restaurant_id), so a
-- venue may appear in as many collections as an editor wants — that is the
-- point of the guide: the same restaurant is legitimately "хорош с детьми" and
-- "лучшие завтраки", and a venue that could sit in only one collection would
-- force editors to pick a favourite rubric per venue. What is forbidden is the
-- same venue TWICE in ONE collection, which the primary key already prevents.
--
-- The unique (collection_id, position) is what makes the order real rather than
-- advisory: two venues cannot claim slot 3. It is DEFERRABLE INITIALLY DEFERRED
-- so a reorder that renumbers rows inside one transaction is not rejected
-- halfway through by an intermediate collision.
--
-- ON DELETE CASCADE on restaurant_id: a deleted venue takes its membership with
-- it, leaving no dangling row for the read to filter. Deactivation is a
-- different, softer thing and is handled by the read (see the repository):
-- is_active = false hides the venue from the collection without an editor
-- losing their curation, because deactivation is routinely temporary.
CREATE TABLE gastroguide_collection_venues
(
    collection_id uuid        NOT NULL REFERENCES gastroguide_collections (id) ON DELETE CASCADE,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    position      integer     NOT NULL,
    -- Editor's line about WHY this venue is in this collection, shown under the
    -- card. Localized like everything else.
    note          varchar     NOT NULL DEFAULT '',
    note_i18n     jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, restaurant_id),
    CONSTRAINT gastroguide_collection_venues_position_unique
        UNIQUE (collection_id, position) DEFERRABLE INITIALLY DEFERRED
);

-- The unique constraint above already indexes (collection_id, position), which
-- is exactly the ordered read — no second index is created for it. This one
-- serves the reverse question ("which collections is this venue in"), needed
-- when a venue is deactivated or removed.
CREATE INDEX idx_gastroguide_collection_venues_restaurant
    ON gastroguide_collection_venues (restaurant_id);

-- +goose Down

DROP TABLE gastroguide_collection_venues;
DROP TABLE gastroguide_collection_categories;
DROP TABLE gastroguide_collections;
DROP TABLE gastroguide_categories;
