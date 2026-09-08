-- +goose Up

-- «Гастропрогулки» — ordered itineraries of the gastroguide (migration 0061).
--
-- WHY THIS IS NOT ANOTHER COLLECTION. A collection is a SET of venues: the
-- order is editorial taste, every member is a catalog row, and dropping one
-- leaves a shorter but still correct list. A route is a SEQUENCE OF STOPS, and
-- the real data from the old system says so plainly: «Классический тур по
-- Алматы» goes Daily Coffee → Парк 28 панфиловцев → Chaihana Palau → Koktobe
-- Terrace. The park is not a venue and never will be, the stop carries its own
-- headline («Утро: Daily Coffee») which is not the venue's name, and stop #3
-- means nothing without #1 and #2. gastroguide_collection_venues cannot hold a
-- non-venue stop (restaurant_id is NOT NULL and part of the primary key) and
-- has nowhere to put a per-stop title, description, photo or coordinates.
--
-- Two tables:
--   gastro_routes        — the route itself, published exactly like a collection
--   gastro_route_points  — the ordered stops
--
-- Everything about publication (status/published_at, the CHECK, the partial
-- index, the position tie-break, the i18n columns, the full-URL images) is
-- copied from gastroguide_collections ON PURPOSE, so a route and a collection
-- behave identically for an editor and for a guest, and neither can drift.

CREATE TABLE gastro_routes
(
    id                  uuid PRIMARY KEY,
    -- slug is the client-facing stable name ("classic-almaty"): the app links
    -- to a route by slug, so it must survive a title rewrite.
    slug                varchar     NOT NULL UNIQUE,
    title               varchar     NOT NULL,
    title_i18n          jsonb,
    description         text        NOT NULL DEFAULT '',
    description_i18n    jsonb,
    -- Full public URL, same convention as collections/promos/events.
    -- NULL = no cover; the API omits it instead of inventing a placeholder.
    cover_image_url     varchar,
    -- The line under the title in the old system: «1 день · 4 точки». It is a
    -- STRING the editor writes, not a computed value: the app shows exactly
    -- what the editorial team wrote («вечер», «2 дня · 7 точек»), and deriving
    -- it from a count would lose the first half of it.
    duration_label      varchar     NOT NULL DEFAULT '',
    duration_label_i18n jsonb,
    -- NULL = shown in every city, same semantics as a collection's city.
    -- Not a FK: there is no cities table (see migration 0061).
    city                varchar,
    status              varchar     NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'published', 'archived')),
    -- Required while published — the guest predicate is then one comparison
    -- with no NULL branch, and scheduled publication comes for free.
    published_at        timestamptz,
    position            integer     NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gastro_routes_published_needs_time
        CHECK (status <> 'published' OR published_at IS NOT NULL)
);

-- Same partial index as the collections' one, for the same reason: the
-- predicate keeps every draft/archived row out and the city filter is left to
-- the heap (routes are counted in tens, and a (city, position) index could not
-- serve the "city IS NULL means everywhere" half of the predicate anyway).
CREATE INDEX idx_gastro_routes_live
    ON gastro_routes (position, id)
    WHERE status = 'published';

-- A STOP ON THE ROUTE.
--
-- The row has its own id, unlike gastroguide_collection_venues whose key is
-- (collection_id, restaurant_id): a stop is not identified by a venue at all —
-- two stops of the same route may both be parks, and the same venue may
-- legitimately open and close a route (coffee in the morning, dinner in the
-- evening).
--
-- kind is the editorial intent, and the two halves of it are constrained
-- differently:
--   'place'      — never carries a restaurant_id (a park has no catalog row);
--   'restaurant' — is CREATED with one (the usecase requires it), but may end
--                  up with NULL if that venue row is later deleted.
--
-- ON DELETE SET NULL, not CASCADE (which is what a collection membership uses):
-- deleting the stop would silently shorten an itinerary whose whole meaning is
-- the sequence, and whose duration_label still says «4 точки». The stop keeps
-- its own title, text, photo and coordinates and simply stops linking to a
-- venue — the same degradation the guest read applies to a DEACTIVATED venue.
CREATE TABLE gastro_route_points
(
    id               uuid PRIMARY KEY,
    route_id         uuid        NOT NULL REFERENCES gastro_routes (id) ON DELETE CASCADE,
    position         integer     NOT NULL,
    kind             varchar     NOT NULL CHECK (kind IN ('restaurant', 'place')),
    restaurant_id    uuid REFERENCES restaurants (id) ON DELETE SET NULL,
    -- The stop's OWN headline («Утро: Daily Coffee»), not the venue's name.
    title            varchar     NOT NULL,
    title_i18n       jsonb,
    description      text        NOT NULL DEFAULT '',
    description_i18n jsonb,
    -- Full public URL of the stop's photo, NULL when the stop has none (a
    -- venue stop then falls back to the venue's catalog image, client-side).
    photo_url        varchar,
    -- The stop's own address and coordinates. A place stop has nowhere else to
    -- get them; a venue stop may leave them empty and the app uses the venue's.
    address          varchar     NOT NULL DEFAULT '',
    address_i18n     jsonb,
    latitude         double precision,
    longitude        double precision,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gastro_route_points_place_has_no_venue
        CHECK (kind <> 'place' OR restaurant_id IS NULL),
    -- Coordinates are both-or-neither: half a pair puts a pin on the equator.
    CONSTRAINT gastro_route_points_coords_both_or_neither
        CHECK ((latitude IS NULL) = (longitude IS NULL)),
    CONSTRAINT gastro_route_points_latitude_range
        CHECK (latitude IS NULL OR (latitude BETWEEN -90 AND 90)),
    CONSTRAINT gastro_route_points_longitude_range
        CHECK (longitude IS NULL OR (longitude BETWEEN -180 AND 180)),
    -- The order is real, not advisory: two stops cannot claim slot 3.
    -- DEFERRABLE INITIALLY DEFERRED so a whole-route renumbering inside one
    -- transaction is not rejected halfway through by an intermediate collision
    -- (same mechanism as gastroguide_collection_venues).
    CONSTRAINT gastro_route_points_position_unique
        UNIQUE (route_id, position) DEFERRABLE INITIALLY DEFERRED
);

-- The unique constraint above already indexes (route_id, position), which is
-- exactly the ordered read. This one serves the ON DELETE SET NULL path:
-- without it Postgres scans every point for every deleted restaurant.
CREATE INDEX idx_gastro_route_points_restaurant
    ON gastro_route_points (restaurant_id)
    WHERE restaurant_id IS NOT NULL;

-- +goose Down

DROP TABLE gastro_route_points;
DROP TABLE gastro_routes;
