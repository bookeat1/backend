-- +goose Up
CREATE TABLE restaurant_favorites
(
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, restaurant_id)
);
-- Speeds up "how many users favorited this restaurant" / cascade lookups;
-- the primary key already covers "list a user's favorites" (user_id first).
CREATE INDEX idx_restaurant_favorites_restaurant ON restaurant_favorites (restaurant_id);

-- +goose Down
DROP TABLE restaurant_favorites;
