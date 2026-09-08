-- +goose Up

-- free_cancel_window_minutes is the per-restaurant "free cancellation" window
-- used by the MONEY path (usecase/payments): a deposit hold is refunded/voided
-- to the guest only when the booking is cancelled EARLIER than this many
-- minutes before starts_at; a later cancellation, or a no-show, forfeits the
-- deposit to the venue (it captures the hold). The owner-confirmed default is
-- 120 minutes.
--
-- Added in two steps (0034 then 0035) so it is safe on a table that already has
-- live rows: this migration adds the column NULLABLE and backfills every
-- existing restaurant to the default, and 0035 then promotes it to NOT NULL
-- with a DEFAULT and a CHECK. Splitting the backfill from the constraint keeps
-- each step trivially reversible and never rejects an in-flight write between
-- the two.
--
-- NOTE (flagged for review): this OVERLAPS the existing, nullable
-- cancel_deadline_minutes (migration 0004), which today governs both the guest
-- self-cancel gate (usecase/bookings) AND the captured-refund settlement
-- resolver. See the PR description — the two windows should be consolidated by
-- the owner; this column is the always-present (NOT NULL) money-decision window
-- the payments layer reads.
ALTER TABLE restaurants
    ADD COLUMN free_cancel_window_minutes integer;

UPDATE restaurants
SET free_cancel_window_minutes = 120
WHERE free_cancel_window_minutes IS NULL;

-- +goose Down
ALTER TABLE restaurants
    DROP COLUMN free_cancel_window_minutes;
