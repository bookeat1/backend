-- +goose Up

-- Restaurant promos (Ф2): a time-boxed offer a venue runs — a happy hour, a
-- seasonal discount. Localized like the catalog (base ru column + optional
-- *_i18n jsonb). starts_at/ends_at is the validity window: the public listing
-- shows a promo only while starts_at <= now < ends_at AND status = 'published'.
-- Enumerated status is VARCHAR validated in Go (internal/domain/promo.go).
CREATE TABLE promos
(
    id               uuid PRIMARY KEY,
    restaurant_id    uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    title            varchar     NOT NULL,
    title_i18n       jsonb,
    description      text        NOT NULL DEFAULT '',
    description_i18n jsonb,
    starts_at        timestamptz NOT NULL,
    ends_at          timestamptz NOT NULL,
    terms            text        NOT NULL DEFAULT '',
    status           varchar     NOT NULL DEFAULT 'draft'
                         CHECK (status IN ('draft', 'published', 'hidden')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT promos_window_valid CHECK (ends_at > starts_at)
);

-- Public listing = a restaurant's published promos whose window contains now.
-- The query filters status='published' AND starts_at <= now AND ends_at > now;
-- this partial index over (restaurant_id, ends_at, starts_at, id) lets Postgres
-- range-scan the still-valid rows and keeps draft/hidden out of the index.
CREATE INDEX idx_promos_active
    ON promos (restaurant_id, ends_at, starts_at, id)
    WHERE status = 'published';

-- Admin cabinet listing = all of a restaurant's promos, newest start first.
CREATE INDEX idx_promos_restaurant_admin
    ON promos (restaurant_id, starts_at DESC, id DESC);

-- +goose Down
DROP TABLE promos;
