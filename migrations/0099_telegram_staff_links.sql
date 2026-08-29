-- +goose Up

-- The Telegram mini app «кабинет ресторана» (spec telegram-miniapp-restaurant.md
-- §5.3): remembers which BookEat account a Telegram account signed in as, so
-- staff type their email and password ONCE and every later open of the mini app
-- goes straight to the shift screen.
--
-- НОМЕР. origin/develop was at 0097 when this was written; 0098 is the venue
-- alert bot migration rebased in the same branch. 0088 (feat/story-expiry),
-- 0089 (this branch, renumbered to 0098) and 0092 (feat/articles-entity) are
-- taken by other open branches. goose runs WITHOUT -allow-missing, so a number
-- below the database's current version is skipped in silence — the reason this
-- one is 0099 and not the first visible gap.
CREATE TABLE IF NOT EXISTS telegram_staff_links (
    -- The Telegram user id, PRIMARY KEY: one Telegram account points at exactly
    -- one BookEat account. Signing in with a different email overwrites user_id
    -- in place rather than adding a second row, so "whose account is this phone
    -- signed into" always has one answer.
    telegram_user_id bigint      PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The private chat the mini app was opened from. Kept so an alert can be
    -- addressed to the person, not only to the venue's shared chat.
    chat_id          bigint      NOT NULL,
    linked_at        timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz,
    -- revoked_at instead of DELETE: staff who lost access ask why, and a row
    -- that is simply gone cannot answer. Active means revoked_at IS NULL.
    revoked_at       timestamptz
);

-- NO UNIQUE (user_id) on purpose: one person may open the mini app from a phone
-- and from a tablet, and both links must keep working. This index serves the
-- reverse question — "revoke every device of this employee" — which runs when
-- their last venue membership disappears.
CREATE INDEX IF NOT EXISTS idx_telegram_staff_links_user
    ON telegram_staff_links (user_id);

-- The venue is DELIBERATELY not stored here. Rights are read live from
-- restaurant_managers on every request; a copy in this table would become a
-- second source of truth about access and would keep a fired employee signed in.

-- +goose Down

DROP TABLE IF EXISTS telegram_staff_links;
