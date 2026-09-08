-- +goose Up

-- Telegram notification channel (increment 2 of the notification backbone).
--
-- Two changes, both safe on a populated DB:
--
--   1. restaurant_notification_settings gains a per-restaurant Telegram target
--      (telegram_chat_id) and a channel toggle (telegram_enabled). A MISSING
--      settings row still means "defaults" — telegram has no chat id yet, so the
--      channel simply has nowhere to send and no-ops. telegram_enabled defaults
--      to true (same convention as web_push_enabled): the effective gate in
--      increment 1 is whether a chat id has been connected.
--
--   2. The dedupe ledger notification_deliveries generalises its target column
--      from "a push subscription" to "any channel target". Web push targets a
--      subscription id; telegram targets the restaurant's chat (keyed by the
--      restaurant id). The (outbox_event_id, channel, target_id) unique key is
--      unchanged in shape — the column is renamed, so the key auto-follows — and
--      still prevents a redelivery from double-sending on any channel.
--
--      The push-subscription FK is dropped: a telegram target is a restaurant,
--      not a push subscription, so the column can no longer reference
--      push_subscriptions. Consequence: when a dead push subscription is deleted
--      its historical delivery markers are no longer cascade-removed and remain
--      as harmless orphan rows (the uuid is never reused, so dedupe correctness
--      is unaffected). booking_outbox's FK on outbox_event_id is kept.

ALTER TABLE restaurant_notification_settings
    ADD COLUMN telegram_chat_id text,
    ADD COLUMN telegram_enabled boolean NOT NULL DEFAULT true;

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_subscription_id_fkey;
ALTER TABLE notification_deliveries
    RENAME COLUMN subscription_id TO target_id;

-- +goose Down

ALTER TABLE notification_deliveries
    RENAME COLUMN target_id TO subscription_id;

-- Before restoring the push-only FK, remove every row it could not accept:
--   1. telegram markers — their subscription_id is a restaurant id, never a
--      push_subscriptions id;
--   2. orphaned web_push markers — the UP migration deliberately dropped the
--      ON DELETE CASCADE, so a delivery row can outlive the subscription it
--      references (the "harmless orphan" case documented above). Those orphans
--      would now violate the restored FK, so they must go too.
DELETE FROM notification_deliveries WHERE channel = 'telegram';
DELETE FROM notification_deliveries
    WHERE subscription_id NOT IN (SELECT id FROM push_subscriptions);

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_subscription_id_fkey
        FOREIGN KEY (subscription_id) REFERENCES push_subscriptions (id) ON DELETE CASCADE;

ALTER TABLE restaurant_notification_settings
    DROP COLUMN telegram_enabled,
    DROP COLUMN telegram_chat_id;
