-- +goose Up

-- RBAC foundation (Ф0): a restaurant staff member now has an explicit role
-- distinguishing the venue's owner from a manager from a hostess, instead of
-- every restaurant_managers row being an undifferentiated "manager". The
-- role→permission matrix itself lives in Go (internal/domain/rbac.go), not
-- here, on purpose (spec: a DB row must never be able to silently grant a
-- permission the code doesn't know about).
--
-- Safe on a table that already has live rows: DEFAULT 'manager' backfills
-- every existing row without a rewrite lock beyond the DDL itself (Postgres
-- 11+ constant-default ADD COLUMN is metadata-only). 'manager' is the
-- conservative choice — every existing manager keeps every capability it
-- already had (including refund, which was previously ungated by role) and
-- only loses staff.manage, which no restaurant_managers row could exercise
-- before this migration anyway (assigning/removing a manager was admin-only).
ALTER TABLE restaurant_managers
    ADD COLUMN role varchar NOT NULL DEFAULT 'manager';

ALTER TABLE restaurant_managers
    ADD CONSTRAINT restaurant_managers_role_check CHECK (role IN ('owner', 'manager', 'hostess'));

-- +goose Down
ALTER TABLE restaurant_managers
    DROP CONSTRAINT restaurant_managers_role_check;

ALTER TABLE restaurant_managers
    DROP COLUMN role;
