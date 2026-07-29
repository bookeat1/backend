-- +goose Up

-- Until now the ONLY way to make somebody a platform administrator was an
-- UPDATE on the users table, typed by hand on the server by whoever had the
-- database password.
--
-- That is a problem in three directions, and this table plus the endpoints
-- above it close all three:
--
--   1. it needed database access, so every promotion was a task for one or two
--      people who might be asleep;
--   2. it left NO trace — nobody could answer "who gave this person admin
--      rights, when, and why", which is the first question asked after
--      something goes wrong;
--   3. it was equally hard to take rights away.
--
-- The audit is a separate table rather than a column on users because a role is
-- a current state while this is a history: users.role answers "what can this
-- person do now", these rows answer "how did it get that way".
CREATE TABLE user_role_changes (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Whose role changed. ON DELETE CASCADE: a deleted account takes its own
    -- history with it, in line with the soft-delete/anonymise rule for users.
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Who changed it. NULL means the platform itself did it — today that is the
    -- bootstrap that promotes the first administrator on an empty system, and
    -- an actor id would be a lie there.
    actor_id    uuid        REFERENCES users(id) ON DELETE SET NULL,
    from_role   varchar     NOT NULL,
    to_role     varchar     NOT NULL,
    -- Free-text why, optional. Kept because "why" is the part a person actually
    -- needs six months later, and it costs nothing to store.
    reason      text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_user_role_changes_user ON user_role_changes (user_id, created_at DESC);
CREATE INDEX idx_user_role_changes_created ON user_role_changes (created_at DESC);

COMMENT ON TABLE user_role_changes IS
  'Audit of global role changes: who changed whose role, from what to what, when and why. A NULL actor_id means the platform itself (bootstrap of the first administrator).';

-- +goose Down

DROP TABLE IF EXISTS user_role_changes;
