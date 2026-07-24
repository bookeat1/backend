-- +goose Up

-- Content drafts (Ф2): the source-agnostic review queue that a future AI
-- content parser (and a hand-entering staff member) writes CANDIDATE events and
-- promos into. It is the human-in-the-loop gate — a draft is born
-- 'pending_review' and NEVER auto-publishes; a staff member with
-- restaurant.manage explicitly approves it (which creates the real published
-- events/promos row, linked back via created_event_id / created_promo_id) or
-- rejects it. Mirrors external_reservations' "one ingestion seam, many sources"
-- shape (see internal/domain/content_draft.go). Enumerated fields are VARCHAR
-- validated in Go, never a Postgres ENUM.
CREATE TABLE content_drafts
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    kind          varchar     NOT NULL CHECK (kind IN ('event', 'promo')),
    source        varchar     NOT NULL,
    -- source_ref = the source system's own id (a post id): a future parser's
    -- dedup key. source_url = a human-openable link back to the original.
    source_ref    varchar,
    source_url    varchar,
    -- raw_payload = the untouched original extraction, kept for audit and so a
    -- reviewer can see exactly what was parsed. Defaults to an empty object.
    raw_payload   jsonb       NOT NULL DEFAULT '{}'::jsonb,

    suggested_title            varchar     NOT NULL DEFAULT '',
    suggested_title_i18n       jsonb,
    suggested_description      text        NOT NULL DEFAULT '',
    suggested_description_i18n jsonb,
    suggested_starts_at        timestamptz,
    suggested_ends_at          timestamptz,
    -- suggested_venue applies to an event draft, suggested_terms to a promo
    -- draft; the irrelevant one for a given kind is simply ignored on approval.
    suggested_venue            varchar,
    suggested_terms            text,

    status           varchar     NOT NULL DEFAULT 'pending_review'
                         CHECK (status IN ('pending_review', 'approved', 'rejected')),
    reviewed_by      uuid        REFERENCES users (id) ON DELETE SET NULL,
    reviewed_at      timestamptz,
    created_event_id uuid        REFERENCES events (id) ON DELETE SET NULL,
    created_promo_id uuid        REFERENCES promos (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- A pending/rejected draft has created nothing; an approved draft created
    -- exactly the entity matching its kind. Enforced so the review outcome and
    -- the created-entity link can never drift out of agreement.
    CONSTRAINT content_drafts_review_consistency CHECK (
        (status = 'pending_review' AND reviewed_by IS NULL AND reviewed_at IS NULL
            AND created_event_id IS NULL AND created_promo_id IS NULL)
        OR (status = 'rejected' AND reviewed_at IS NOT NULL
            AND created_event_id IS NULL AND created_promo_id IS NULL)
        OR (status = 'approved' AND reviewed_at IS NOT NULL
            AND ((kind = 'event' AND created_event_id IS NOT NULL AND created_promo_id IS NULL)
                 OR (kind = 'promo' AND created_promo_id IS NOT NULL AND created_event_id IS NULL)))
    )
);

-- The review queue: a restaurant's pending drafts, oldest first (FIFO) with id
-- as a stable pagination tie-breaker. Partial so approved/rejected history is
-- kept out of the hot listing index.
CREATE INDEX idx_content_drafts_pending
    ON content_drafts (restaurant_id, created_at, id)
    WHERE status = 'pending_review';

-- +goose Down
DROP TABLE content_drafts;
