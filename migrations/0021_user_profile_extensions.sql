-- +goose Up

-- Guest profile fields (roadmap #7/#9/#14): country + birth date for
-- tourist/local analytics, both nullable until the guest fills their profile.
-- Country is stored as an ISO 3166-1 alpha-2 code, validated in app code
-- (golang.org/x/text/language.ParseRegion + Region.IsCountry) rather than a
-- DB CHECK against a hand-maintained list. Avatar already exists
-- (users.avatar_url, added in 0001) — no new column needed there.
ALTER TABLE users
    ADD COLUMN country_code varchar(2),
    ADD COLUMN birth_date   date;

-- Foodie profile: many-to-many link between a user and the restaurants'
-- existing cuisine/category dictionary (restaurant_categories, 0002). No new
-- cuisine dictionary is invented — this reuses the same reference list
-- restaurants themselves are tagged with via restaurants.category_id.
CREATE TABLE user_cuisine_preferences
(
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category_id uuid        NOT NULL REFERENCES restaurant_categories (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, category_id)
);
CREATE INDEX idx_user_cuisine_preferences_user ON user_cuisine_preferences (user_id);

-- +goose Down
DROP TABLE user_cuisine_preferences;
ALTER TABLE users
    DROP COLUMN birth_date,
    DROP COLUMN country_code;
