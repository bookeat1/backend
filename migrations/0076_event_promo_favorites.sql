-- +goose Up

-- Favorites for EVENTS and PROMOS, next to the venue favorites that already
-- exist (restaurant_favorites, migration 0020).
--
-- Until now the heart on an «Афиша» card had nowhere to write to, so the app
-- favorited the event's RESTAURANT instead: saving a wine dinner silently
-- bookmarked the venue, and un-saving it removed a venue the guest may have
-- saved on purpose. These two tables give each kind its own home; the venue
-- table and every read that hangs off it are untouched.
--
-- Two decisions worth stating up front:
--
-- 1. A recurring event is favorited as a SERIES, not as one date.
--    The guest catalog already collapses a series into ONE card — its nearest
--    upcoming occurrence (migration 0074 + EventRepository.ListPublicUpcoming).
--    The guest therefore never sees "this Wednesday" as a separate thing to
--    save; they see «Cocktail Wednesday». Storing the tapped occurrence id
--    would mean the saved item rots into a past date the moment that Wednesday
--    passes, while the very same card is still live in the Афиша. So a
--    favorite on a generated occurrence is stored against its rule
--    (recurrence_id) and the read resolves it forward to whichever occurrence
--    is next. A one-off event is still stored as itself.
--
--    Hence the exactly-one-of shape below rather than a plain (user, event)
--    pair: the two targets are genuinely different objects, and the pair of
--    partial unique indexes is what makes "save the same series twice" a
--    database-enforced no-op instead of an application-level check.
--
-- 2. A favorite never outlives its target. Both FKs cascade, so deleting an
--    event, a promo, a rule or a user takes the bookmarks with it — there is no
--    orphan row that a read could turn into a card opening onto a 404.
--    Withdrawal that is NOT a delete (status back to draft/hidden, the window
--    closing, the venue deactivated) keeps the row and is filtered out at READ
--    time only: a re-published promo comes back to the guest's screen with the
--    heart still on it, which a delete-on-unpublish would have thrown away.

CREATE TABLE event_favorites
(
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Exactly one of the two is set (see the CHECK): a one-off event, or the
    -- rule behind a recurring one. recurrence_id CASCADEs, unlike
    -- events.recurrence_id which is ON DELETE SET NULL — an occurrence must
    -- survive its rule (it may carry sold tickets), a bookmark of a series that
    -- no longer exists must not.
    event_id      uuid REFERENCES events (id) ON DELETE CASCADE,
    recurrence_id uuid REFERENCES event_recurrences (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT event_favorites_exactly_one_target
        CHECK (num_nonnulls(event_id, recurrence_id) = 1)
);

-- Idempotency, enforced by the DATABASE and not by a read-then-insert check:
-- one row per (user, one-off event) and one row per (user, series). Saving the
-- same event twice — or two different Wednesdays of the same series — conflicts
-- on one of these and the INSERT's ON CONFLICT DO NOTHING turns it into a
-- silent no-op. Two partial indexes rather than one over both columns, because
-- a single index over a nullable pair would treat NULLs as distinct and let the
-- duplicate through.
CREATE UNIQUE INDEX uniq_event_favorites_event
    ON event_favorites (user_id, event_id)
    WHERE event_id IS NOT NULL;

CREATE UNIQUE INDEX uniq_event_favorites_series
    ON event_favorites (user_id, recurrence_id)
    WHERE recurrence_id IS NOT NULL;

-- The favorites screen's only scan: one user's rows, most recently saved first.
CREATE INDEX idx_event_favorites_user
    ON event_favorites (user_id, created_at DESC);

CREATE TABLE promo_favorites
(
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    promo_id   uuid        NOT NULL REFERENCES promos (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- A promo has no series concept, so the plain pair IS the identity and the
    -- primary key gives idempotency for free — same shape as
    -- restaurant_favorites.
    PRIMARY KEY (user_id, promo_id)
);

CREATE INDEX idx_promo_favorites_user
    ON promo_favorites (user_id, created_at DESC);

-- +goose Down

-- Both tables are new and nothing references them, so the rollback is a plain
-- drop: it loses the bookmarks themselves and touches no other table's rows.
DROP TABLE promo_favorites;
DROP TABLE event_favorites;
