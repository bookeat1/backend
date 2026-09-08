-- +goose Up

-- Merchandising feed (the app's main-screen "Акции" rail): venues supply the
-- content — promos and events — and WE decide what is shown and in which order.
--
-- Deliberately NO new promo/event entity and NO separate placement table: a
-- feed slot is a PROPERTY of the item the venue already created, so the feed
-- read is a plain UNION ALL over promos+events with no polymorphic join (a
-- placement table would need an untyped item_id with no real FK, and would make
-- the single-query guest read a three-way join). The cost of this choice is
-- that one item can hold exactly one placement — if the platform ever wants
-- several concurrent campaigns per promo, that table becomes necessary.
--
-- The columns are identical on both tables on purpose: the union query, the
-- CAS transition and the scan helper are then one shared shape.
--
--   feed_status lifecycle (VARCHAR validated in Go, never a Postgres ENUM —
--   internal/domain/feed.go), mirroring the content-draft review vocabulary:
--     not_submitted  -- the venue has not asked for the main screen (default)
--     pending_review -- the venue submitted it; awaiting a PLATFORM decision
--     approved       -- the platform said yes; eligible for the main screen
--     rejected       -- the platform said no (feed_rejection_reason says why);
--                       the venue may fix the item and submit again
--
-- The venue's own publication status (status = draft/published/hidden) is a
-- SEPARATE axis and both must be green: an item reaches the feed only when
-- status='published' AND feed_status='approved' AND its window still contains
-- now. Hiding an item therefore pulls it off the main screen instantly without
-- touching the moderation decision.
--
-- feed_placement_weight is the paid-placement lever, set by the superadmin only
-- (0 = organic). It is the dominant ranking signal but not an absolute one —
-- see domain.ScoreFeedItem.
--
-- Safe on live rows: every column is nullable or has a constant DEFAULT, so
-- Postgres applies them without rewriting the table.

ALTER TABLE promos
    ADD COLUMN feed_status            varchar     NOT NULL DEFAULT 'not_submitted'
        CHECK (feed_status IN ('not_submitted', 'pending_review', 'approved', 'rejected')),
    ADD COLUMN feed_submitted_at      timestamptz,
    -- ON DELETE SET NULL: deleting the reviewer's account must not delete the
    -- moderation decision, only who made it.
    ADD COLUMN feed_reviewed_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN feed_reviewed_at       timestamptz,
    ADD COLUMN feed_rejection_reason  text,
    ADD COLUMN feed_placement_weight  integer     NOT NULL DEFAULT 0
        CHECK (feed_placement_weight BETWEEN 0 AND 100);

ALTER TABLE events
    ADD COLUMN feed_status            varchar     NOT NULL DEFAULT 'not_submitted'
        CHECK (feed_status IN ('not_submitted', 'pending_review', 'approved', 'rejected')),
    ADD COLUMN feed_submitted_at      timestamptz,
    ADD COLUMN feed_reviewed_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN feed_reviewed_at       timestamptz,
    ADD COLUMN feed_rejection_reason  text,
    ADD COLUMN feed_placement_weight  integer     NOT NULL DEFAULT 0
        CHECK (feed_placement_weight BETWEEN 0 AND 100);

-- Guest main-screen read: only published+approved rows, ordered inside each
-- table by the SQL pre-order the repository uses (paid weight first, then the
-- soonest deadline, then id as the stable tie-break) so the union's candidate
-- window needs no sort node per branch. The partial predicate keeps every
-- draft/hidden/unapproved row out of the index entirely.
CREATE INDEX idx_promos_feed_live
    ON promos (feed_placement_weight DESC, ends_at, id)
    WHERE status = 'published' AND feed_status = 'approved';

CREATE INDEX idx_events_feed_live
    ON events (feed_placement_weight DESC, ends_at, id)
    WHERE status = 'published' AND feed_status = 'approved';

-- Platform moderation queue: the superadmin's "what is waiting for me" list,
-- oldest submission first (a review queue is FIFO) with id as the tie-break.
CREATE INDEX idx_promos_feed_review_queue
    ON promos (feed_submitted_at, id)
    WHERE feed_status = 'pending_review';

CREATE INDEX idx_events_feed_review_queue
    ON events (feed_submitted_at, id)
    WHERE feed_status = 'pending_review';

-- +goose Down

DROP INDEX idx_events_feed_review_queue;
DROP INDEX idx_promos_feed_review_queue;
DROP INDEX idx_events_feed_live;
DROP INDEX idx_promos_feed_live;

ALTER TABLE events
    DROP COLUMN feed_placement_weight,
    DROP COLUMN feed_rejection_reason,
    DROP COLUMN feed_reviewed_at,
    DROP COLUMN feed_reviewed_by,
    DROP COLUMN feed_submitted_at,
    DROP COLUMN feed_status;

ALTER TABLE promos
    DROP COLUMN feed_placement_weight,
    DROP COLUMN feed_rejection_reason,
    DROP COLUMN feed_reviewed_at,
    DROP COLUMN feed_reviewed_by,
    DROP COLUMN feed_submitted_at,
    DROP COLUMN feed_status;
