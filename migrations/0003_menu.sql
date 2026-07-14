-- +goose Up
CREATE TABLE menu_categories
(
    id            uuid PRIMARY KEY,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    parent_id     uuid REFERENCES menu_categories (id) ON DELETE SET NULL,
    display_order integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_menu_categories_parent ON menu_categories (parent_id);

CREATE TABLE menu_items
(
    id                 uuid PRIMARY KEY,
    restaurant_id      uuid           NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name               varchar        NOT NULL,
    name_i18n          jsonb,
    description        varchar        NOT NULL DEFAULT '',
    description_i18n   jsonb,
    price              numeric(12, 2) NOT NULL DEFAULT 0,
    image_url          varchar,
    is_available       boolean        NOT NULL DEFAULT true,
    category           varchar,
    category_i18n      jsonb,
    subcategory        varchar,
    subcategory_i18n   jsonb,
    portion_size       varchar,
    portion_size_i18n  jsonb,
    language           varchar,
    display_order      integer,
    created_at         timestamptz    NOT NULL DEFAULT now(),
    updated_at         timestamptz    NOT NULL DEFAULT now()
);
CREATE INDEX idx_menu_items_listing ON menu_items (restaurant_id, display_order, name);
CREATE INDEX idx_menu_items_language ON menu_items (restaurant_id, language);

CREATE TABLE menu_item_tags
(
    id           uuid PRIMARY KEY,
    menu_item_id uuid        NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    tag          varchar     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (menu_item_id, tag)
);
CREATE INDEX idx_menu_item_tags_item ON menu_item_tags (menu_item_id);

-- +goose Down
DROP TABLE menu_item_tags;
DROP TABLE menu_items;
DROP TABLE menu_categories;
