-- +goose Up
-- The booking worker claims due rows with
--   WHERE status = ANY(...) AND <created_at|ends_at> < cutoff
--   ORDER BY <same column> LIMIT n FOR UPDATE SKIP LOCKED
-- (see internal/infrastructure/postgres/booking/repository.go, ClaimDue).
--
-- idx_bookings_status_starts (status, starts_at) does not serve that: it forces
-- a sort on a column the WHERE clause no longer filters on. Ordering by the
-- cutoff column is what keeps the oldest waiting booking from being starved out
-- of every batch, so it needs its own index.
CREATE INDEX idx_bookings_status_created ON bookings (status, created_at);
CREATE INDEX idx_bookings_status_ends ON bookings (status, ends_at);

-- +goose Down
DROP INDEX IF EXISTS idx_bookings_status_ends;
DROP INDEX IF EXISTS idx_bookings_status_created;
