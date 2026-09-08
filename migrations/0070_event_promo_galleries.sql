-- +goose Up

-- GALLERIES FOR EVENTS AND PROMOS.
--
-- WHY. An event and a promo each carry exactly ONE picture today
-- (events.cover_image_url / promos.cover_image_url, added by 0060). The
-- «Статья» design shows an event inside a collection as a ROW of photos, and
-- the event card itself is meant to show more than the cover. One column
-- cannot hold an ordered set, so each gets a child table — the same shape
-- restaurant_images already uses for the venue gallery, deliberately: a reader
-- who knows one knows the others.
--
-- The cover STAYS where it is. It is what the cards, the feed and every
-- existing client read, and moving it into the gallery would break all three at
-- once for no gain. The gallery is additive: cover first, gallery after it.
--
-- position is the editor's order, not the upload order — the panel reorders by
-- drag and writes the new numbers. Ties fall back to created_at so a set
-- written with equal positions still renders in a stable, explainable order
-- instead of whatever the planner happens to return.
--
-- ON DELETE CASCADE: a photo of a deleted event is not a thing that exists.
-- Both parents are already hard-deletable (promos and events are content, not
-- ledger rows), so the child must go with them rather than dangle.

CREATE TABLE event_images
(
    id         uuid PRIMARY KEY,
    event_id   uuid        NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    image_url  varchar     NOT NULL,
    position   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_event_images_event ON event_images (event_id, position, created_at);

CREATE TABLE promo_images
(
    id         uuid PRIMARY KEY,
    promo_id   uuid        NOT NULL REFERENCES promos (id) ON DELETE CASCADE,
    image_url  varchar     NOT NULL,
    position   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_promo_images_promo ON promo_images (promo_id, position, created_at);

-- +goose Down

DROP TABLE promo_images;
DROP TABLE event_images;
