-- +goose Up

-- Second payments review fixes (2026-07-23), non-blocking item #1: VoidOnRejection
-- used to call the acquirer's /cancel BEFORE claiming anything locally, unlike
-- CaptureOnSeating (which claims `authorized -> capturing` first, see 0008).
-- That asymmetry meant two concurrent hold-release requests for the SAME
-- booking (a double click, a retried request) could both reach the acquirer.
--
-- 'voiding' is the symmetric transient claim state for VoidOnRejection: only
-- the CAS winner of `authorized -> voiding` may call gw.Void; the loser gets
-- ErrAlreadyExists and must not call it too. It must count as "live" for the
-- same reason 'capturing' does — otherwise a second payment could be
-- authorized for the same booking while the first is mid-void. Recreated
-- rather than altered in place: Postgres has no ALTER INDEX ... ADD CONDITION.
DROP INDEX idx_payments_live_per_booking;
CREATE UNIQUE INDEX idx_payments_live_per_booking
    ON payments (booking_id)
    WHERE status IN ('authorized', 'capturing', 'voiding', 'captured');

-- +goose Down
DROP INDEX idx_payments_live_per_booking;
CREATE UNIQUE INDEX idx_payments_live_per_booking
    ON payments (booking_id)
    WHERE status IN ('authorized', 'capturing', 'captured');
