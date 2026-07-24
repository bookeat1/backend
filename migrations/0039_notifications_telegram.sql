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

-- Drop telegram delivery markers first: their target_id is a restaurant id, not
-- a push_subscriptions id, so restoring the FK would otherwise fail on them.
DELETE FROM notification_deliveries WHERE channel = 'telegram';

ALTER TABLE notification_deliveries
    RENAME COLUMN target_id TO subscription_id;
ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_subscription_id_fkey
        FOREIGN KEY (subscription_id) REFERENCES push_subscriptions (id) ON DELETE CASCADE;

ALTER TABLE restaurant_notification_settings
    DROP COLUMN telegram_enabled,
    DROP COLUMN telegram_chat_id;
