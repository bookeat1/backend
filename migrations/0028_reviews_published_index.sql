-- +goose Up

-- Serves the two public read paths, both of which only ever touch PUBLISHED
-- reviews of one restaurant, so a partial index keeps hidden (moderated) rows
-- out of it entirely:
--   * the paginated listing — ORDER BY created_at DESC, id DESC is a stable,
--     tie-broken total order (two reviews in the same instant never swap
--     places across pages), and the index carries that exact order so no
--     sort node is needed;
--   * the aggregate rating (AVG(rating), COUNT(*)) — the leading
--     restaurant_id lets Postgres scan only that venue's published rows.
-- The UNIQUE (restaurant_id, user_id) constraint from 0027 covers the guest's
-- own-review lookup, so no separate index is needed for that path.
CREATE INDEX idx_reviews_published_listing
    ON reviews (restaurant_id, created_at DESC, id DESC)
    WHERE status = 'published';

-- +goose Down
DROP INDEX idx_reviews_published_listing;
