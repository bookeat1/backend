-- +goose Up

-- legacy_sync_cursor is the per-entity high-water mark of the one-way sync that
-- pulls the OLD BookEat system (live Supabase Postgres, still serving guests)
-- into this backend. One row per synced entity. The worker resumes from
-- last_synced_at / last_synced_id after a restart instead of rescanning the
-- whole source, and advances the cursor only after a batch has been written.
--
-- The cursor is a (timestamp, id) pair, compared as a row value, so two source
-- rows sharing the same updated_at are still walked deterministically and
-- exactly once. NULL last_synced_id means "nothing synced yet" — the default
-- 'epoch' timestamp then matches every source row on the first tick.
--
-- Nothing in the request path depends on this table: it is written and read
-- exclusively by cmd/worker's legacy-sync loop. When LEGACY_DB_URL is unset the
-- loop never starts and this table simply stays empty.
CREATE TABLE legacy_sync_cursor
(
    entity         varchar     PRIMARY KEY,
    last_synced_at timestamptz NOT NULL DEFAULT 'epoch',
    last_synced_id uuid,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE legacy_sync_cursor;
