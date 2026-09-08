-- +goose Up
-- Fairness for the shared notification outbox.
--
-- Until now the dispatcher claimed `WHERE published_at IS NULL ORDER BY
-- created_at LIMIT 100` and an event was published only when EVERY interested
-- channel returned nil. One channel with a sustained outage (WhatsApp/Meta:
-- rate limits, expiring tokens) therefore parked its events at the HEAD of the
-- queue: once ~100 of them accumulated, the batch was full of permanently
-- failing rows and newer events were never claimed at all. A WhatsApp outage
-- silently took down Telegram, web push, guest push and the in-app feed.
--
-- The fix is a per-event attempt counter with backoff: a failed event is moved
-- to the BACK of the queue (next_attempt_at in the future) instead of being
-- re-read every tick, and after the attempt budget is exhausted it is
-- explicitly abandoned rather than retried forever or quietly published.
--
-- Safe on a live table: all four columns are nullable or have a constant
-- default (Postgres 11+ rewrites no rows for `ADD COLUMN ... DEFAULT 0`), so
-- existing rows keep their meaning — attempts = 0, next_attempt_at = NULL
-- ("due now"), abandoned_at = NULL ("still ours to deliver").
ALTER TABLE booking_outbox
    ADD COLUMN attempts        integer NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at timestamptz,
    ADD COLUMN last_error      text,
    ADD COLUMN abandoned_at    timestamptz;

COMMENT ON COLUMN booking_outbox.attempts IS
    'Delivery attempts made so far. 0 = never attempted.';
COMMENT ON COLUMN booking_outbox.next_attempt_at IS
    'Earliest time this event may be claimed again. NULL = due now.';
COMMENT ON COLUMN booking_outbox.last_error IS
    'Why the last attempt failed. NULL = no attempt has failed.';
COMMENT ON COLUMN booking_outbox.abandoned_at IS
    'Set when the attempt budget ran out. The dead letter: the row is no longer '
        'claimed, is NOT published, and needs a human. Requeue by clearing '
        'abandoned_at/attempts/next_attempt_at — the notification_deliveries '
        'ledger keeps the replay from double-notifying anyone.';

-- The claim predicate changed shape (it now also excludes abandoned rows and
-- filters on next_attempt_at), so the old partial index no longer covers it.
-- Both statements touch only booking_outbox, which is drained continuously and
-- therefore short; the exclusive lock is momentary.
DROP INDEX idx_booking_outbox_unpublished;
CREATE INDEX idx_booking_outbox_due ON booking_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL AND abandoned_at IS NULL;

-- Standing dead-letter question, kept next to the data it answers:
--   SELECT id, booking_id, event_type, attempts, last_error, abandoned_at
--     FROM booking_outbox WHERE abandoned_at IS NOT NULL ORDER BY abandoned_at DESC;

-- +goose Down
-- Rolling back loses the dead letter: a row that was abandoned becomes an
-- ordinary unpublished event again and the old dispatcher will retry it
-- forever. That is the pre-0083 behaviour by definition — export
-- `WHERE abandoned_at IS NOT NULL` first if the rows still matter.
DROP INDEX idx_booking_outbox_due;
CREATE INDEX idx_booking_outbox_unpublished ON booking_outbox (created_at) WHERE published_at IS NULL;
ALTER TABLE booking_outbox
    DROP COLUMN abandoned_at,
    DROP COLUMN last_error,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts;
