-- +goose Up

-- analytics_cursor is the per-source high-water mark of the Amplitude analytics
-- worker (cmd/worker's analytics loop). One row per source outbox. The worker
-- ships product events to Amplitude by re-reading the EXISTING transactional
-- outboxes (booking_outbox, payment_outbox) rather than owning its own outbox:
-- those rows are already written in the same transaction as the business
-- mutation, so "the booking happened but analytics missed it" is impossible.
--
-- Why a cursor and not published_at: booking_outbox is already drained by the
-- notification dispatcher, which owns its published_at marker as the sole
-- drainer. Analytics must read the same rows WITHOUT competing for that marker,
-- so it tracks its own independent (created_at, id) position instead. The two
-- consumers never contend.
--
-- The cursor is a (timestamp, id) pair compared as a row value, so two source
-- rows sharing the same created_at are still walked deterministically and
-- exactly once (same discipline as legacy_sync_cursor). The worker advances the
-- cursor only AFTER a batch was accepted by Amplitude; a failed send leaves the
-- cursor where it was and the batch is reshipped next tick (Amplitude dedupes
-- on device_id + insert_id within 7 days, so a reship never double-counts).
--
-- Seeded to now() on both sources so a fresh deploy starts analytics at
-- deploy time and does NOT flood Amplitude with the pre-existing outbox
-- backlog. Nothing in the request path reads or writes this table.
CREATE TABLE analytics_cursor
(
    source          varchar(32) PRIMARY KEY,
    last_created_at timestamptz NOT NULL DEFAULT 'epoch',
    last_id         uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    updated_at      timestamptz NOT NULL DEFAULT now()
);

INSERT INTO analytics_cursor (source, last_created_at, last_id)
VALUES ('booking_outbox', now(), '00000000-0000-0000-0000-000000000000'),
       ('payment_outbox', now(), '00000000-0000-0000-0000-000000000000');

-- +goose Down
DROP TABLE analytics_cursor;
