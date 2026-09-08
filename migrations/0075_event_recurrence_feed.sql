-- +goose Up

-- The home feed for RECURRING events.
--
-- Migration 0074 gave a rule the publication status its occurrences are born
-- with (occurrence_status), but nothing for the SECOND visibility axis the feed
-- reads: `events.feed_status` (migration 0050). Every generated occurrence
-- therefore landed as 'not_submitted' and the main screen — which shows only
-- status='published' AND feed_status='approved' — never saw a single one, while
-- the hand-made events inserted next to them did show.
--
-- This migration gives the rule that missing piece, and it deliberately gives
-- it the SAME shape the per-item moderation already has:
--
--   occurrence_feed_status  — the platform's decision about the SERIES, in the
--                             same vocabulary as promos.feed_status /
--                             events.feed_status (domain.FeedStatus);
--   feed_submitted_at       — when the venue last asked for the main screen;
--   feed_reviewed_by/_at    — which superadmin decided, and when;
--   feed_rejection_reason   — mandatory on a refusal.
--
-- Why the decision lives on the RULE and not on each occurrence: an eight-week
-- window of a daily rule is ~56 rows. Submitting each of them individually
-- would put 56 identical cards in the moderation queue for one editorial
-- decision, and the moderator would be re-reading the same text every week
-- forever. The reviewed object is the series; the occurrences inherit its
-- verdict. NOTHING here lets a venue approve its own content: the venue may
-- only move the rule to 'pending_review' (feed submit), and only a platform
-- superadmin may move it to 'approved' or 'rejected' — exactly the gate a
-- single promo or event goes through today.
--
-- Default 'not_submitted' keeps every rule that already exists behaving
-- literally as it does now: occurrences keep being born out of the feed until
-- somebody asks for it. The column is nullable-free but has a DEFAULT, so on a
-- populated table this is a catalog-only rewrite-free ADD COLUMN.

SET lock_timeout = '3s';

ALTER TABLE event_recurrences
    ADD COLUMN occurrence_feed_status varchar NOT NULL DEFAULT 'not_submitted'
        CHECK (occurrence_feed_status IN ('not_submitted', 'pending_review', 'approved', 'rejected')),
    ADD COLUMN feed_submitted_at      timestamptz,
    ADD COLUMN feed_reviewed_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN feed_reviewed_at       timestamptz,
    ADD COLUMN feed_rejection_reason  text;

RESET lock_timeout;

-- The superadmin's rule queue: oldest submission first, id as the stable
-- tie-break. Same shape (and same reasoning) as idx_events_feed_review_queue.
CREATE INDEX idx_event_recurrences_feed_queue
    ON event_recurrences (feed_submitted_at, id)
    WHERE occurrence_feed_status = 'pending_review';

-- The feed decision on a rule is applied to the occurrences it already
-- generated (approve → its future occurrences become approved, withdraw →
-- back out of the feed). That write selects by recurrence_id among the events
-- that have not ended yet, which is precisely this index.
CREATE INDEX idx_events_recurrence_feed_sync
    ON events (recurrence_id, ends_at)
    WHERE recurrence_id IS NOT NULL;

-- +goose Down

DROP INDEX idx_events_recurrence_feed_sync;
DROP INDEX idx_event_recurrences_feed_queue;

-- Dropping the columns loses only the series-level moderation decision; every
-- occurrence keeps the feed_status it currently carries on its own row, so a
-- rollback never pulls an approved card off the main screen.
ALTER TABLE event_recurrences
    DROP COLUMN feed_rejection_reason,
    DROP COLUMN feed_reviewed_at,
    DROP COLUMN feed_reviewed_by,
    DROP COLUMN feed_submitted_at,
    DROP COLUMN occurrence_feed_status;
