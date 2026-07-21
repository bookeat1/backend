# ETL: Supabase dump → clean schema

One-time, idempotent migration of the Supabase data into the clean domain
schema. Every subcommand upserts by the original `id`, so re-running is safe.

## Steps

1. Migrate the clean schema: `go run ./cmd/migrate/migrate.go up`
2. Create the `raw_supabase` staging schema and load the Supabase dump into it
   (`auth.users` → `raw_supabase.users`, `public.*` → `raw_supabase.*`).
3. Run the subcommands **in this order** (each depends on the previous ones'
   foreign keys):

```bash
go run ./cmd/etl/main.go users
go run ./cmd/etl/main.go restaurants
go run ./cmd/etl/main.go menu
go run ./cmd/etl/main.go bookings
```

## users

Joins auth users with profiles on `id`, preserves the original UUID, normalizes
phones to E.164 (`+7` default), and copies bcrypt password hashes verbatim so
existing passwords keep working.

## restaurants / menu

Plain `INSERT … SELECT` upserts in foreign-key order. Rows whose parent was not
migrated are dropped by the joins.

## bookings

Migrates `bookings`, `booking_tables`, `booking_items`, `booking_messages`,
`booking_blacklist`, `booking_rate_log` and `restaurant_surveys`
(spec `docs/superpowers/specs/2026-07-21-bookings-domain-design.md`, §8).

Mapping rules:

- `booking_date` → `starts_at`; `ends_at = starts_at + duration`, where the
  duration is the venue's `restaurants.booking_duration_minutes` when set and
  `BOOKING_DEFAULT_DURATION_MINUTES` (90) otherwise. Real visit lengths were
  never stored, so historical rows get the current policy.
- The legacy single `bookings.table_id` is expanded into a `booking_tables`
  row whose `slot` includes the venue's buffer on both sides. Its id is derived
  deterministically from (booking, table) so a re-run upserts the same row.
  `active` is set from the status at insert time (only `pending` / `confirmed` /
  `arrived` hold a table) — otherwise historical rows would fight the exclusion
  constraint.
- `booking_items.item_price` (numeric) → `item_price_minor` (bigint tiyn,
  × 100, banker's rounding), `currency = 'KZT'`.
- `phone` → `phone_normalized` via `internal/auth/phone`; emails are
  lower-cased.
- Statuses are copied as-is. `no_show` is **never** back-filled — Supabase has
  no data to tell a no-show from a forgotten booking.
- `promotion_id` / `event_id` are copied without a foreign key on purpose.

Rows that cannot be placed are skipped, not fatal: a missing restaurant, a
missing `booking_date`, an unknown status, a table that no longer exists, or a
legacy slot that overlaps an already-migrated active booking. `user_id` values
absent from `users` are dropped (the booking becomes a guest booking) instead of
losing the row.

The run finishes with a per-table summary of migrated vs skipped rows.
