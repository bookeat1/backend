-- +goose Up
CREATE TABLE cuisines
(
    id         uuid PRIMARY KEY,
    name       text        NOT NULL,
    image_url  text,
    sort       integer     NOT NULL DEFAULT 0,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_cuisines_listing ON cuisines (is_active, sort, name);

CREATE TABLE promotions
(
    id             uuid PRIMARY KEY,
    restaurant_id  uuid REFERENCES restaurants (id) ON DELETE SET NULL,
    title          text        NOT NULL,
    discount_label text,
    starts_at      timestamptz,
    ends_at        timestamptz,
    image_url      text,
    is_active      boolean     NOT NULL DEFAULT true,
    sort           integer     NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_promotions_listing ON promotions (is_active, sort);
CREATE INDEX idx_promotions_window ON promotions (starts_at, ends_at);
CREATE INDEX idx_promotions_restaurant ON promotions (restaurant_id);

CREATE TABLE articles
(
    id           uuid PRIMARY KEY,
    title        text        NOT NULL,
    author_label text,
    cover_url    text,
    url          text,
    published_at timestamptz,
    is_active    boolean     NOT NULL DEFAULT true,
    sort         integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_articles_listing ON articles (is_active, published_at DESC, sort);

-- +goose Down
DROP TABLE articles;
DROP TABLE promotions;
DROP TABLE cuisines;
