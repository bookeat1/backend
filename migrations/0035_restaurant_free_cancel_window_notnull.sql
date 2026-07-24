-- +goose Up

-- Promote free_cancel_window_minutes (added and backfilled in 0034) to its
-- final shape: a non-negative, always-present setting with the owner-confirmed
-- default of 120 minutes. Running this only after 0034's backfill means no
-- existing row can violate the NOT NULL at the moment it is applied.
ALTER TABLE restaurants
    ALTER COLUMN free_cancel_window_minutes SET DEFAULT 120,
    ALTER COLUMN free_cancel_window_minutes SET NOT NULL,
    ADD CONSTRAINT chk_restaurants_free_cancel_window_nonneg
        CHECK (free_cancel_window_minutes >= 0);

-- +goose Down
ALTER TABLE restaurants
    DROP CONSTRAINT chk_restaurants_free_cancel_window_nonneg,
    ALTER COLUMN free_cancel_window_minutes DROP NOT NULL,
    ALTER COLUMN free_cancel_window_minutes DROP DEFAULT;
