-- +goose Up

-- A COLLECTION BLOCK MAY NOW POINT AT AN EVENT OR A PROMO.
--
-- WHY. In the «Статья» design a block is not a bare venue card: it is headed
-- «Еженедельное событие в Mongol Bar», carries a row of photos, then the
-- event's own title and description, and only at the bottom the venue's address
-- and Instagram. In other words the block is a venue PLUS the thing happening
-- there.
--
-- WHY NOT A NEW ITEMS TABLE. The venue stays the anchor of the block — the
-- address, the Instagram and the link to the restaurant screen all come from
-- it, in the design too. So this is one more fact about an existing row, not a
-- new kind of row: two nullable references, and the block renders the event's
-- (or promo's) title, text and gallery on top of the venue it already had.
-- A separate items table would have forced every read, the ordering and the
-- editor UI to handle three shapes to express the same picture.
--
-- Both are nullable and at most ONE may be set: a block illustrated by an event
-- AND a promo at once has no meaning in the design, and the check refuses it at
-- the schema rather than leaving each reader to pick a winner.
--
-- ON DELETE SET NULL, not CASCADE: deleting an event must not silently drop the
-- venue out of the editor's collection. The block falls back to the plain venue
-- card — the same thing it renders today — and the editor sees it and decides.

ALTER TABLE gastroguide_collection_venues
    ADD COLUMN event_id uuid REFERENCES events (id) ON DELETE SET NULL,
    ADD COLUMN promo_id uuid REFERENCES promos (id) ON DELETE SET NULL,
    ADD CONSTRAINT gastroguide_collection_venues_one_highlight
        CHECK (event_id IS NULL OR promo_id IS NULL);

-- Both indexes serve the ON DELETE SET NULL path: without them Postgres scans
-- the whole link table for every deleted event or promo.
CREATE INDEX idx_guide_collection_venues_event ON gastroguide_collection_venues (event_id)
    WHERE event_id IS NOT NULL;
CREATE INDEX idx_guide_collection_venues_promo ON gastroguide_collection_venues (promo_id)
    WHERE promo_id IS NOT NULL;

-- +goose Down

DROP INDEX idx_guide_collection_venues_promo;
DROP INDEX idx_guide_collection_venues_event;
ALTER TABLE gastroguide_collection_venues
    DROP CONSTRAINT gastroguide_collection_venues_one_highlight,
    DROP COLUMN promo_id,
    DROP COLUMN event_id;
