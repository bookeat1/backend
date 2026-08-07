-- +goose Up

-- Restaurant "stories" are Instagram-highlight-style promo cards a venue pins to
-- its storefront: a picture, an optional one-line caption, shown in a horizontal
-- rail in the mobile app. They are a pure read surface for the guest side (the
-- venue cabinet that fills them is a separate task), so this migration only
-- introduces the table the public list endpoint reads from.
--
-- image_url is the FULL public URL, exactly like restaurant_images.image_url,
-- menu_items.image_url and events.cover_image_url — today that is the Cloudflare
-- R2 managed domain (see ADR-013). A story without a picture makes no sense, so
-- image_url is NOT NULL; caption is optional (a wordless highlight card is a
-- valid product state), so it is nullable with no default — NULL means "no
-- caption", the response omits it.
--
-- ON DELETE CASCADE mirrors every other venue-scoped catalog table (menu_items,
-- events, reviews): a venue removed from the platform takes its stories with it,
-- never leaves orphan rows the guest app could still surface.
CREATE TABLE restaurant_stories (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    image_url     text        NOT NULL,
    caption       text,
    sort_order    integer     NOT NULL DEFAULT 0,
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- The one query this table serves is "active stories of this restaurant, in
-- display order" (ORDER BY sort_order, created_at, id). The index carries every
-- column that read touches — restaurant_id + is_active for the filter, then all
-- three sort keys — so it is served as an index-ordered scan with no separate
-- sort node, and inactive rows stay out of the way of it. id is included as the
-- deterministic final tie-break (now() is constant per transaction, so bulk
-- inserts share created_at).
CREATE INDEX idx_restaurant_stories_listing
    ON restaurant_stories (restaurant_id, is_active, sort_order, created_at, id);

-- +goose Down

DROP TABLE IF EXISTS restaurant_stories;
