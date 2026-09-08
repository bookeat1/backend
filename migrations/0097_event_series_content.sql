-- +goose Up

-- Shared content for a recurring series: fill the poster in ONCE, not once per
-- date.
--
-- Migration 0074 made every occurrence a full copy of the rule's template, so
-- «Афиша Greek Party» is eighteen rows in `events` carrying eighteen copies of
-- the same title, the same text and the same cover. Changing a word means
-- eighteen edits, which is exactly the complaint this migration answers.
--
-- Three decisions, stated up front:
--
-- 1. The shared content lives on the RULE (event_recurrences), which already
--    has these columns and already holds the series-level moderation decision
--    (0075). No new table, no new parent object, and — this is the part that
--    matters on a live database — NOT A SINGLE events.id changes. Bookings and
--    tickets point at occurrence ids; a design that replaced the occurrences
--    with one "template event" would have had to move those references.
--
-- 2. The occurrences keep their own copy of the words; the rule's editor pushes
--    changes down (SyncOccurrenceContent). The alternative — resolving content
--    through a join at read time — would mean rewriting every read path there
--    is (guest listing, collapse, detail, home feed, tickets, notifications),
--    and one missed query shows a guest last month's poster. One careful
--    writer beats a dozen careful readers.
--
-- 3. "Inherited" and "deliberately empty" are told apart by an explicit LIST of
--    the fields a date owns, not by NULL. NULL cannot carry that meaning here:
--    a NULL cover_image_url already means "no cover" and an empty description
--    already means "no description" for every one-off event in the table. A
--    field named in content_overrides belongs to the date, whatever its value —
--    including an empty one; a field that is absent follows the series.

SET lock_timeout = '3s';

-- NOT NULL with a DEFAULT is a catalog-only change on PG 11+, so this is safe
-- on a populated table: no rewrite, no long lock.
ALTER TABLE events
    ADD COLUMN content_overrides text[] NOT NULL DEFAULT '{}';

RESET lock_timeout;

-- The vocabulary is closed, and the database is where it is closed: a typo
-- ('cover' instead of 'cover_image_url') would otherwise silently mean "this
-- field is not overridden" and the date would lose its own poster on the next
-- series edit. Same list as domain.EventContentFields.
--
-- NOT VALID + VALIDATE: the check is enforced for every new write immediately,
-- and the existing rows — which all carry '{}' from the DEFAULT — are verified
-- afterwards without holding an exclusive lock for the scan.
ALTER TABLE events
    ADD CONSTRAINT events_content_overrides_known
        CHECK (content_overrides <@ ARRAY ['title', 'description', 'venue', 'cover_image_url', 'tags']::text[])
        NOT VALID;
ALTER TABLE events
    VALIDATE CONSTRAINT events_content_overrides_known;

-- There is deliberately NO check tying a non-empty content_overrides to a
-- non-null recurrence_id, tempting as it is. events.recurrence_id is
-- ON DELETE SET NULL (0074): deleting a rule nulls the column on every one of
-- its occurrences, and a CHECK is re-evaluated on exactly that UPDATE — it
-- would turn "delete this rule" into a constraint violation. An orphaned
-- occurrence keeps a stale marker list instead, which is inert: nothing
-- inherits into a row with no rule.

-- ---------------------------------------------------------------------------
-- BACKFILL: the six series that already exist on production.
--
-- The rule's template is not necessarily what the venue actually publishes:
-- occurrences were hand-edited after generation, and on some rules the template
-- was never filled in properly at all. So the series content is taken from the
-- BEST-FILLED occurrence of each rule (ties broken by the earliest date, then
-- by id, so the choice is deterministic and a re-run picks the same row), and
-- every other occurrence keeps its own content — marked, field by field, as an
-- override wherever it differs from what the series now says.
--
-- Nothing in `events` loses a single character here: this backfill only WRITES
-- the marker column and READS the content. The rule's own template is the only
-- thing overwritten, and its previous value is kept for the rollback below.
-- ---------------------------------------------------------------------------

CREATE TABLE event_recurrence_content_backup_0097
(
    recurrence_id    uuid PRIMARY KEY REFERENCES event_recurrences (id) ON DELETE CASCADE,
    title            varchar NOT NULL,
    title_i18n       jsonb,
    description      text    NOT NULL,
    description_i18n jsonb,
    venue            varchar NOT NULL,
    cover_image_url  varchar,
    tags             text[]  NOT NULL
);

COMMENT ON TABLE event_recurrence_content_backup_0097 IS
    'Rule templates as they were before migration 0097 overwrote them with the '
        'best-filled occurrence''s content. Read only by the Down of 0097; safe to '
        'drop once that rollback is no longer wanted.';

INSERT INTO event_recurrence_content_backup_0097
SELECT id, title, title_i18n, description, description_i18n, venue, cover_image_url, tags
FROM event_recurrences;

-- The anchor: one occurrence per rule, the one with the most content filled in.
-- +goose StatementBegin
WITH anchor AS (SELECT DISTINCT ON (e.recurrence_id) e.recurrence_id,
                                                     e.title,
                                                     e.title_i18n,
                                                     e.description,
                                                     e.description_i18n,
                                                     e.venue,
                                                     e.cover_image_url,
                                                     e.tags
                FROM events e
                WHERE e.recurrence_id IS NOT NULL
                ORDER BY e.recurrence_id,
                         (
                             (btrim(e.title) <> '')::int +
                             (btrim(e.description) <> '')::int +
                             (btrim(e.venue) <> '')::int +
                             (e.cover_image_url IS NOT NULL AND btrim(e.cover_image_url) <> '')::int +
                             (cardinality(e.tags) > 0)::int +
                             (e.title_i18n IS NOT NULL)::int +
                             (e.description_i18n IS NOT NULL)::int
                             ) DESC,
                         e.starts_at ASC,
                         e.id ASC)
UPDATE event_recurrences r
SET title            = a.title,
    title_i18n       = a.title_i18n,
    description      = a.description,
    description_i18n = a.description_i18n,
    venue            = a.venue,
    cover_image_url  = a.cover_image_url,
    tags             = a.tags,
    updated_at       = now()
FROM anchor a
WHERE r.id = a.recurrence_id
  -- A rule whose anchor has an EMPTY title keeps its own: title is NOT NULL and
  -- carries the cabinet's only handle on the series.
  AND btrim(a.title) <> '';
-- +goose StatementEnd

-- Every date that differs from what the series now says keeps its own content,
-- and says so. The anchor itself ends up with '{}' — it IS the series content —
-- without being special-cased: it simply differs in nothing.
--
-- Past occurrences are marked too. They are never rewritten by a sync
-- (ends_at > now bounds it), but a marker that says the truth costs nothing and
-- survives any later change to that bound.
-- +goose StatementBegin
UPDATE events e
SET content_overrides = (SELECT coalesce(array_agg(f ORDER BY ord), '{}'::text[])
                         FROM (VALUES ('title'::text, 1,
                                       e.title IS DISTINCT FROM r.title OR
                                       e.title_i18n IS DISTINCT FROM r.title_i18n),
                                      ('description', 2,
                                       e.description IS DISTINCT FROM r.description OR
                                       e.description_i18n IS DISTINCT FROM r.description_i18n),
                                      ('venue', 3, e.venue IS DISTINCT FROM r.venue),
                                      ('cover_image_url', 4, e.cover_image_url IS DISTINCT FROM r.cover_image_url),
                                      ('tags', 5, e.tags IS DISTINCT FROM r.tags)) AS v(f, ord, differs)
                         WHERE differs)
FROM event_recurrences r
WHERE e.recurrence_id = r.id;
-- +goose StatementEnd

-- +goose Down

-- Down loses nothing a guest or a venue can see.
--
-- The occurrences are not touched at all: their content was never rewritten by
-- the Up, so every date still carries exactly the words it carries now. Only
-- the marker column disappears, and with it the ability to tell "inherited"
-- from "owned" — which is precisely the state the schema was in before 0097.
--
-- The rules get their pre-0097 templates back from the backup table. A rule
-- created AFTER 0097 has no backup row and keeps whatever template it has,
-- which is correct: nothing overwrote it.
UPDATE event_recurrences r
SET title            = b.title,
    title_i18n       = b.title_i18n,
    description      = b.description,
    description_i18n = b.description_i18n,
    venue            = b.venue,
    cover_image_url  = b.cover_image_url,
    tags             = b.tags,
    updated_at       = now()
FROM event_recurrence_content_backup_0097 b
WHERE r.id = b.recurrence_id;

DROP TABLE event_recurrence_content_backup_0097;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_content_overrides_known;
ALTER TABLE events
    DROP COLUMN content_overrides;
