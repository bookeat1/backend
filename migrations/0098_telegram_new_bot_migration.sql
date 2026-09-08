-- +goose Up

-- Staged migration of venue booking alerts from the old notifications bot to
-- @book_eat_restaurants_bot (spec telegram-miniapp-restaurant.md §7).
--
-- The chat id itself does NOT change: a Telegram chat id is the same number for
-- every bot, so restaurant_notification_settings.telegram_chat_id stays valid.
-- What changes is the RIGHT to write there — a new bot may not message a user
-- who never pressed Start, nor a group it was never added to. These two columns
-- record, per venue, whether that right has been established:
--
--   telegram_new_bot_ready_at   the new bot successfully reached this chat's
--                               owner (staff pressed /start, or the bot was
--                               added to the group). NULL = not migrated yet,
--                               keep using the old bot.
--   telegram_new_bot_failed_at  the last time the new bot was refused
--                               (400/403). Written together with resetting
--                               ready_at, so a venue that removes the bot falls
--                               back to the old one automatically.
--
-- Both are nullable with no default: safe on a populated table (no rewrite, no
-- lock beyond the catalog update), and "everyone starts unmigrated" is exactly
-- the semantics we want.

ALTER TABLE restaurant_notification_settings
    ADD COLUMN telegram_new_bot_ready_at  timestamptz,
    ADD COLUMN telegram_new_bot_failed_at timestamptz;

-- The migration dashboard asks the same question every day: "which venues have
-- a chat connected but are not on the new bot yet?". A partial index keeps that
-- scan cheap and stays tiny (it shrinks to nothing as the migration completes).
CREATE INDEX IF NOT EXISTS idx_rns_telegram_new_bot_pending
    ON restaurant_notification_settings (restaurant_id)
    WHERE telegram_chat_id IS NOT NULL AND telegram_new_bot_ready_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_rns_telegram_new_bot_pending;

ALTER TABLE restaurant_notification_settings
    DROP COLUMN telegram_new_bot_failed_at,
    DROP COLUMN telegram_new_bot_ready_at;
