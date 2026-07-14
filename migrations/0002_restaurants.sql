-- +goose Up
CREATE TABLE restaurant_categories
(
    id               uuid PRIMARY KEY,
    name             varchar     NOT NULL,
    name_i18n        jsonb,
    description      varchar,
    description_i18n jsonb,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE restaurants
(
    id                   uuid PRIMARY KEY,
    category_id          uuid REFERENCES restaurant_categories (id) ON DELETE SET NULL,
    name                 varchar          NOT NULL,
    name_i18n            jsonb,
    description          varchar          NOT NULL DEFAULT '',
    description_i18n     jsonb,
    cuisine_type         varchar          NOT NULL DEFAULT '',
    cuisine_type_i18n    jsonb,
    address              varchar          NOT NULL DEFAULT '',
    address_i18n         jsonb,
    opening_hours        varchar          NOT NULL DEFAULT '',
    opening_hours_i18n   jsonb,
    city                 varchar          NOT NULL,
    price_category       varchar          NOT NULL,
    email                varchar          NOT NULL DEFAULT '',
    phone                varchar          NOT NULL DEFAULT '',
    latitude             double precision,
    longitude            double precision,
    kwaaka_restaurant_id varchar,
    is_active            boolean          NOT NULL DEFAULT true,
    is_new               boolean,
    is_popular           boolean,
    is_premium           boolean,
    hidden_from_home     boolean          NOT NULL DEFAULT false,
    display_order        integer,
    created_at           timestamptz      NOT NULL DEFAULT now(),
    updated_at           timestamptz      NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurants_listing ON restaurants (is_active, display_order, name);
CREATE INDEX idx_restaurants_category ON restaurants (category_id);

CREATE TABLE restaurant_features
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_features_rid ON restaurant_features (restaurant_id);

CREATE TABLE restaurant_images
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    image_url     varchar     NOT NULL,
    is_primary    boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_images_rid_primary ON restaurant_images (restaurant_id, is_primary);

CREATE TABLE restaurant_tags
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    tag_name      varchar     NOT NULL,
    tag_name_i18n jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_tags_rid ON restaurant_tags (restaurant_id);

CREATE TABLE restaurant_social_links
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    type          varchar     NOT NULL,
    url           varchar     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_social_links_rid ON restaurant_social_links (restaurant_id);

CREATE TABLE restaurant_working_hours
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    day_of_week   integer     NOT NULL,
    open_time     varchar,
    close_time    varchar,
    is_open       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_working_hours_rid ON restaurant_working_hours (restaurant_id);

CREATE TABLE restaurant_time_slots
(
    id                   uuid PRIMARY KEY,
    restaurant_id        uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    day_of_week          integer     NOT NULL,
    start_time           varchar     NOT NULL,
    end_time             varchar     NOT NULL,
    is_manually_disabled boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_time_slots_rid ON restaurant_time_slots (restaurant_id);

CREATE TABLE restaurant_tables
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          varchar     NOT NULL,
    capacity      integer     NOT NULL DEFAULT 0,
    description   varchar,
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_tables_rid ON restaurant_tables (restaurant_id);

CREATE TABLE restaurant_floor_plans
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    layout_data   jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_restaurant_floor_plans_rid ON restaurant_floor_plans (restaurant_id);

CREATE TABLE restaurant_managers
(
    id             uuid PRIMARY KEY,
    restaurant_id  uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_by     uuid,
    whatsapp_opt_in boolean    NOT NULL DEFAULT false,
    whatsapp_phone varchar,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (restaurant_id, user_id)
);
CREATE INDEX idx_restaurant_managers_user ON restaurant_managers (user_id);

CREATE TABLE restaurant_partnership_requests
(
    id              uuid PRIMARY KEY,
    restaurant_name varchar     NOT NULL,
    contact_name    varchar     NOT NULL,
    email           varchar     NOT NULL,
    phone           varchar     NOT NULL,
    address         varchar     NOT NULL DEFAULT '',
    cuisine_type    varchar,
    description     varchar,
    additional_info varchar,
    status          varchar     NOT NULL DEFAULT 'pending',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE restaurant_partnership_requests;
DROP TABLE restaurant_managers;
DROP TABLE restaurant_floor_plans;
DROP TABLE restaurant_tables;
DROP TABLE restaurant_time_slots;
DROP TABLE restaurant_working_hours;
DROP TABLE restaurant_social_links;
DROP TABLE restaurant_tags;
DROP TABLE restaurant_images;
DROP TABLE restaurant_features;
DROP TABLE restaurants;
DROP TABLE restaurant_categories;
