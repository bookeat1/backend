-- +goose Up

-- Restaurant admin panel (Ф1): the venue "guest list" endpoint aggregates the
-- bookings table grouped by (restaurant_id, phone_normalized) — phone_normalized
-- is the stable guest identity a booking always carries, even for a guest with
-- no account. The existing indexes cover restaurant_id+starts_at (the booking
-- calendar) and phone_normalized alone (blacklist/rate-log lookups), but neither
-- serves "distinct guests OF this restaurant" efficiently. This composite index
-- lets the grouped scan stay inside one restaurant's rows ordered by the group
-- key, instead of scanning every booking of the venue and hash-aggregating.
--
-- Safe on a live table: CREATE INDEX takes a brief lock but adds no column and
-- rewrites no data; it is a pure read-path optimization. (Not CONCURRENTLY —
-- goose runs each migration in a transaction, and the deploy runbook applies
-- migrations against the venue before app traffic is switched over, so the
-- ordinary locking build is acceptable here.)
CREATE INDEX idx_bookings_restaurant_phone
    ON bookings (restaurant_id, phone_normalized);

-- +goose Down
DROP INDEX IF EXISTS idx_bookings_restaurant_phone;
