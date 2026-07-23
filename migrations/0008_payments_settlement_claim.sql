-- +goose Up

-- Payments review fixes (2026-07-23, see the payments-usecase QA report,
-- item #7): a terminal settlement marker on the payment itself.
--
-- Why this is needed: a late-cancellation or a no-show settlement legitimately
-- does NOT move Payment.status away from 'captured' (the venue already holds
-- what it is entitled to from capture time, see 0007's ledger design). That
-- means status alone cannot answer "has this payment already been settled?",
-- and without an explicit marker a second RefundUseCase.Settle call — with a
-- DIFFERENT trigger and a DIFFERENT client idempotency key — sails straight
-- through the "status == captured" precondition and refunds the guest a
-- second time on top of what the venue already kept.
ALTER TABLE payments
    ADD COLUMN settled_at                  timestamptz,
    ADD COLUMN settled_trigger             varchar,
    ADD COLUMN settlement_idempotency_key  varchar;

-- The CAS that RefundUseCase.Settle relies on is a single
-- `UPDATE payments SET settled_at = ... WHERE id = $1 AND settled_at IS NULL`;
-- no index is required for that (it is a single-row primary-key update), but a
-- partial index makes "which payments still need a settlement" queries (a
-- reporting / reconciliation view) cheap without scanning captured payments
-- that were never cancelled at all.
CREATE INDEX idx_payments_settled ON payments (settled_at) WHERE settled_at IS NOT NULL;

-- Report item #5: CaptureOnSeating now claims the hold with a
-- `authorized -> capturing` CAS BEFORE calling the acquirer, so a second,
-- concurrent CaptureOnSeating for the same booking loses the race instead of
-- also calling Capture. 'capturing' must count as "live" for the same reason
-- 'authorized'/'captured' do — otherwise a second payment could be authorized
-- for the same booking while the first is mid-capture. Recreated rather than
-- altered in place: Postgres has no ALTER INDEX ... ADD CONDITION.
DROP INDEX idx_payments_live_per_booking;
CREATE UNIQUE INDEX idx_payments_live_per_booking
    ON payments (booking_id)
    WHERE status IN ('authorized', 'capturing', 'captured');

-- +goose Down
DROP INDEX idx_payments_live_per_booking;
CREATE UNIQUE INDEX idx_payments_live_per_booking
    ON payments (booking_id)
    WHERE status IN ('authorized', 'captured');

DROP INDEX idx_payments_settled;

ALTER TABLE payments
    DROP COLUMN settled_at,
    DROP COLUMN settled_trigger,
    DROP COLUMN settlement_idempotency_key;
