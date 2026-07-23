-- +goose Up

-- Soft delete for guest accounts (roadmap #10): the row is kept — bookings and
-- payments keep referencing users.id unchanged for accounting/history — but
-- internal/infrastructure/postgres/user.Repository.Delete anonymizes the
-- personal columns (email/phone/full_name/avatar_url/city/country_code/
-- birth_date) and flips is_active false in the same write that sets
-- deleted_at. Email/phone go to NULL rather than a placeholder string
-- specifically because both columns are UNIQUE and Postgres never considers
-- two NULLs equal, so the phone/email become reusable by a new signup right
-- away with no constraint change needed here.
ALTER TABLE users
    ADD COLUMN deleted_at timestamptz;

-- +goose Down
ALTER TABLE users
    DROP COLUMN deleted_at;
