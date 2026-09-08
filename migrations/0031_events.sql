-- +goose Up

-- Restaurant events (Ф2): a one-off happening a venue hosts — a wine dinner, a
-- live-music night. Localized like the catalog: a base scalar column (ru) plus
-- an optional *_i18n jsonb map (see internal/domain/restaurant.go I18n.Resolve).
-- Enumerated status is VARCHAR validated in Go (internal/domain/event.go), never
-- a Postgres ENUM, same convention as every other table here.
--
-- ticketed / ticket_price_minor / capacity are carried as FIELDS ONLY in this
-- increment: the ticket purchase + payment flow is a deliberately deferred
-- follow-up. ticket_price_minor is integer minor units (tiyin/cents), never a
-- float, consistent with every money value in this codebase.
CREATE TABLE events
(
    id                 uuid PRIMARY KEY,
    restaurant_id      uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    title              varchar     NOT NULL,
    title_i18n         jsonb,
    description        text        NOT NULL DEFAULT '',
    description_i18n   jsonb,
    starts_at          timestamptz NOT NULL,
    ends_at            timestamptz NOT NULL,
    venue              varchar     NOT NULL DEFAULT '',
    cover_image_url    varchar,
    status             varchar     NOT NULL DEFAULT 'draft'
                           CHECK (status IN ('draft', 'published', 'hidden')),
    ticketed           boolean     NOT NULL DEFAULT false,
    -- integer minor units; NULL when the event is free / not ticketed.
    ticket_price_minor bigint      CHECK (ticket_price_minor IS NULL OR ticket_price_minor >= 0),
    capacity           integer     CHECK (capacity IS NULL OR capacity >= 0),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- An event's window must be non-empty: it cannot end before it starts.
    CONSTRAINT events_window_valid CHECK (ends_at > starts_at)
);

-- Public listing = a restaurant's published events that have not ended yet,
-- soonest first with id as a stable pagination tie-breaker. The partial index
-- keeps draft/hidden rows out entirely and carries the exact order the query
-- uses (ORDER BY starts_at ASC, id ASC) so no sort node is needed.
CREATE INDEX idx_events_published_upcoming
    ON events (restaurant_id, starts_at, id)
    WHERE status = 'published';

-- Admin cabinet listing = all of a restaurant's events, newest start first.
CREATE INDEX idx_events_restaurant_admin
    ON events (restaurant_id, starts_at DESC, id DESC);

-- +goose Down
DROP TABLE events;
