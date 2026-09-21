-- +goose Up

-- GUEST-FACING BOOKING-RULES COPY (Trello BNjLdfSP): "explicit rules at
-- booking time and in the reminder". A venue may optionally override two
-- pieces of copy shown next to the booking confirmation and in the pre-visit
-- reminder:
--
--   hold_minutes        — how long the table is held after the booked time
--                          before it is released. Platform default: 15
--                          (BOOKING_DEFAULT_HOLD_MINUTES).
--   late_arrival_text   — a short note on what to do when running late
--                          ("Опаздываете — позвоните в заведение."). Platform
--                          default: BOOKING_DEFAULT_LATE_ARRIVAL_TEXT.
--   late_arrival_text_i18n — translations of late_arrival_text, same
--                          convention as every other *_i18n column since
--                          migration 0101: the 'ru' entry equals the plain
--                          column, the invariant is held by the write gateway
--                          (usecase/restaurants), not a DB CHECK.
--
-- The THIRD rule the card mentions — free cancellation — is deliberately NOT a
-- new column here. It already exists as the MONEY path,
-- restaurants.free_cancel_window_minutes (migration 0034/0035), which
-- usecase/payments enforces for the actual deposit-forfeiture decision and
-- which the venue's own manager already edits via the admin-panel free-cancel
-- endpoint. A second, independently-editable "free_cancel_hours" column would
-- let this footer promise a window the money path does not actually honour —
-- exactly the kind of guest-trust bug this feature exists to prevent. The
-- guest-facing hours figure is derived (minutes / 60) from that same column —
-- see usecase/restaurants.ResolveBookingRules.
--
-- NULLABLE, NO DB DEFAULT ON PURPOSE (both columns): the platform default
-- lives in code (bootstrap.Config / BOOKING_DEFAULT_*), never in the schema,
-- so it can be changed without a migration. NULL means "this venue has not
-- overridden the platform default".
--
-- SAFE ON LIVE ROWS: two nullable columns with no default plus one nullable
-- jsonb are a catalog-only change — on PG 11+ this is a metadata-only ALTER,
-- no table rewrite, no long lock. Every existing row reads back exactly as it
-- did before (both NULL = "use the platform default", the same answer the
-- application gave with no column at all).
SET lock_timeout = '3s';

ALTER TABLE restaurants
    ADD COLUMN hold_minutes integer,
    ADD COLUMN late_arrival_text text,
    ADD COLUMN late_arrival_text_i18n jsonb,
    ADD CONSTRAINT restaurants_hold_minutes_positive
        CHECK (hold_minutes IS NULL OR hold_minutes > 0),
    ADD CONSTRAINT restaurants_late_arrival_text_i18n_needs_text
        CHECK (late_arrival_text_i18n IS NULL OR late_arrival_text IS NOT NULL);

COMMENT ON COLUMN restaurants.hold_minutes IS
  'How long (minutes) this venue holds a table after the booked time before releasing it. NULL = use the platform default (BOOKING_DEFAULT_HOLD_MINUTES). Resolved by usecase/restaurants.ResolveBookingRules.';
COMMENT ON COLUMN restaurants.late_arrival_text IS
  'Short guest-facing note on what to do when running late, shown at booking confirmation and in the pre-visit reminder. NULL = use the platform default (BOOKING_DEFAULT_LATE_ARRIVAL_TEXT).';
COMMENT ON COLUMN restaurants.late_arrival_text_i18n IS
  'Translations of late_arrival_text: {"kk": …, "en": …}. Key ''ru'' equals the column (invariant held by usecase/restaurants, not a CHECK — same convention as migration 0101). NULL = no translations, or no text set at all.';

-- +goose Down

ALTER TABLE restaurants
    DROP CONSTRAINT IF EXISTS restaurants_late_arrival_text_i18n_needs_text,
    DROP CONSTRAINT IF EXISTS restaurants_hold_minutes_positive,
    DROP COLUMN IF EXISTS late_arrival_text_i18n,
    DROP COLUMN IF EXISTS late_arrival_text,
    DROP COLUMN IF EXISTS hold_minutes;
