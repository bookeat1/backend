-- +goose Up

-- Idempotency keys for mutating endpoints (spec §7: POST /bookings requires an
-- Idempotency-Key header).
--
-- Design:
--   * the row is inserted in the SAME transaction as the booking it describes,
--     so "booking exists" and "key is taken" can never disagree;
--   * uniqueness is scoped to (user_id, endpoint, idempotency_key): a key is a
--     client-chosen string, so two users may legitimately pick the same one,
--     and one user may reuse a key across different endpoints;
--   * request_hash is a SHA-256 of the raw request body. The same key with a
--     different body is a client bug (or an attack) and is rejected with 409
--     rather than silently replaying an unrelated response;
--   * response holds the stored success payload, replayed verbatim on a retry.
--
-- Retention: rows are only useful for the client's retry window. A periodic
-- cleanup (`DELETE FROM idempotency_keys WHERE created_at < now() - interval
-- '24 hours'`) is the intended maintenance; the created_at index supports it.
CREATE TABLE idempotency_keys
(
    id              uuid PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint        varchar     NOT NULL,
    idempotency_key varchar     NOT NULL,
    request_hash    varchar     NOT NULL,
    response        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_idempotency_keys UNIQUE (user_id, endpoint, idempotency_key)
);

CREATE INDEX idx_idempotency_keys_created_at ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
