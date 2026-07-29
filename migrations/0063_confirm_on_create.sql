-- +goose Up

-- auto_confirm used to answer two different questions with one column:
--   1. is a NEW booking confirmed the instant it is made?
--   2. what happens when the confirm SLA elapses and the venue never answered?
--
-- That made the arrangement venues actually want unreachable. Turning instant
-- confirmation off (so staff can accept or decline) also turned off the safety
-- net, and a guest whose venue never opened the panel stayed pending forever —
-- the worker only escalated once and left the booking hanging.
--
-- The two questions are now two columns:
--   confirm_on_create — skip the venue and confirm immediately. Default OFF.
--   auto_confirm      — confirm a pending booking once the SLA elapses. Unchanged.
--
-- Both are NULLABLE per-venue overrides of an env default, exactly like the
-- other booking-policy columns: NULL means "use the platform default".
ALTER TABLE restaurants
  ADD COLUMN confirm_on_create boolean;

COMMENT ON COLUMN restaurants.confirm_on_create IS
  'Per-venue override: confirm a new booking immediately instead of letting the venue answer it. NULL = platform default (BOOKING_DEFAULT_CONFIRM_ON_CREATE, off). Distinct from auto_confirm, which only decides what happens after the confirm SLA elapses.';

-- Venues that explicitly asked for instant confirmation keep it. Only those
-- rows: a NULL auto_confirm means the venue never chose, and those venues move
-- to the new default (the venue answers) together with everybody else.
UPDATE restaurants
   SET confirm_on_create = true
 WHERE auto_confirm IS TRUE;

-- +goose Down

ALTER TABLE restaurants DROP COLUMN IF EXISTS confirm_on_create;
