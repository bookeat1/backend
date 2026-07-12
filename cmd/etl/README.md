# ETL: Supabase dump → clean users domain

One-time, idempotent migration of identity data.

## Steps

1. Migrate the clean schema: `go run ./cmd/migrate/migrate.go up`
2. Create staging schema and load the Supabase dump so that `auth.users` →
   `raw_supabase.users` and `public.profiles` → `raw_supabase.profiles`.
3. Run: `go run ./cmd/etl/main.go`

The command joins auth users with profiles on `id`, preserves the original
UUID, normalizes phones to E.164 (`+7` default), and copies bcrypt password
hashes verbatim so existing passwords keep working. Re-running is safe (upsert
by `id`).
