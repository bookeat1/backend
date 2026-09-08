-- +goose Up

-- Guest-facing notifications (increment 3 of the notification backbone).
--
-- Everything shipped so far addresses STAFF: push_subscriptions is keyed by a
-- staff user_id + restaurant_id, and both channels (web push, Telegram) alert a
-- venue about incoming bookings. The GUEST got nothing — no confirmation, no
-- reminder, no cancellation notice — which the terms and the privacy policy
-- already promise them. Two additions close that gap:
--
--   device_push_tokens          — a signed-in guest's MOBILE push token (the app
--                                 is Expo/React Native, so an Expo push token in
--                                 practice; the column is a plain text token so
--                                 an FCM/APNs token fits the same row when the
--                                 sender is swapped). Deliberately NOT scoped by
--                                 restaurant_id: unlike a staff subscription,
--                                 the guest's device is notified about the
--                                 guest's OWN bookings wherever they are.
--
--   bookings.guest_reminder_sent_at — the pre-visit reminder's idempotency
--                                 marker. The reminder pass stamps it and emits
--                                 the outbox event in ONE transaction, so a
--                                 reminder is emitted at most once per booking
--                                 even across a worker restart.
--
-- Delivery itself reuses the EXISTING machinery: the reminder is a booking
-- outbox event, drained by the same notification dispatcher, deduped by the same
-- notification_deliveries ledger (target_id = the device token row's id).

CREATE TABLE device_push_tokens
(
    id         uuid PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The provider push token. One device = one token: re-registering an
    -- existing token RE-POINTS it to the current account instead of creating a
    -- second row (a shared phone, or a guest signing in on a friend's device,
    -- must not keep delivering to the previous owner).
    token      text        NOT NULL,
    -- "ios" | "android" | "web" — free VARCHAR validated in app code, never a DB
    -- enum (same discipline as consent_type / booking status).
    platform   varchar(16) NOT NULL,
    -- A dead token is DEACTIVATED, never deleted: the delivery ledger references
    -- this row's id as its target, and keeping the row keeps that history
    -- readable. Reactivation is just the upsert flipping it back to true.
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_device_push_tokens_token UNIQUE (token)
);

-- Fan-out lookup: "every live device of THIS guest". Partial on is_active
-- because the sender never reads a deactivated row.
CREATE INDEX idx_device_push_tokens_user_active
    ON device_push_tokens (user_id) WHERE is_active;

ALTER TABLE bookings
    ADD COLUMN guest_reminder_sent_at timestamptz;

-- Backfill, in two parts, both safe on a populated table:
--
--   1. A booking the OLD Supabase system already reminded (its legacy
--      reminder_60_sent_at / reminder_30_sent_at columns are set, carried over
--      by the legacy sync) inherits that timestamp — otherwise the same guest
--      would be reminded twice during the migration window, once by each system.
--
--   2. Every other booking whose visit is still in the FUTURE is stamped with
--      now(). Rationale mirrors the analytics cursor seeded at now(): the new
--      reminder pass must react to bookings made from here on, not fire a salvo
--      at guests whose visit happens to fall inside the reminder window at
--      deploy time (the old system is still reminding those). A booking whose
--      starts_at is already in the past can never be claimed by the pass (it
--      only looks forward), so it needs no stamp and the UPDATE stays small.
UPDATE bookings
SET guest_reminder_sent_at = COALESCE(reminder_30_sent_at, reminder_60_sent_at)
WHERE reminder_30_sent_at IS NOT NULL
   OR reminder_60_sent_at IS NOT NULL;

UPDATE bookings
SET guest_reminder_sent_at = now()
WHERE guest_reminder_sent_at IS NULL
  AND starts_at > now();

-- The reminder pass's claim predicate: upcoming, not yet reminded. Partial on
-- "not yet reminded" so the index shrinks as bookings are processed, and it
-- never has to scan the stamped backlog.
CREATE INDEX idx_bookings_guest_reminder_due
    ON bookings (starts_at) WHERE guest_reminder_sent_at IS NULL;

-- +goose Down

DROP INDEX idx_bookings_guest_reminder_due;
ALTER TABLE bookings
    DROP COLUMN guest_reminder_sent_at;
DROP TABLE device_push_tokens;
