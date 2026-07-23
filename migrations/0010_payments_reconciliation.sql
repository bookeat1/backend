-- +goose Up

-- Reconciliation worker (internal/usecase/payments.Reconciler). Review
-- finding: three transient states have no automatic way out —
-- `capturing`/`voiding` (a process died between claiming the hold action and
-- the acquirer answering), `payment_refunds.in_flight`/`pending` (same, plus
-- an acquirer answer that was genuinely a timeout/5xx) — and a lost webhook
-- can leave `created`/`authorized` stale while the acquirer already moved the
-- money. The acquirer's own Get() is the only source of truth for all of
-- these; this migration adds the "lease, not a lock forever" bookkeeping the
-- worker needs to tell a payment that is stuck from one that is simply being
-- worked on right now by a normal, fast request.

-- status_changed_at is the lease clock: stamped every time `status` actually
-- changes (CompareAndSwapStatus / UpdateStatus), never by any other write.
-- created_at cannot serve this purpose — a payment authorized three days ago
-- that just entered `capturing` a second ago looks identical to one stuck in
-- `capturing` for three days if the only clock available is created_at.
--
-- reconcile_attempts / last_reconcile_attempt_at / needs_manual_review are the
-- avalanche guard: a payment the worker could not resolve N times in a row is
-- flagged and the worker stops calling the acquirer for it (see
-- RecordReconcileAttempt's doc comment in internal/domain/payment.go).
--
-- Backfill: every existing row's status_changed_at becomes the latest
-- lifecycle timestamp it already has, falling back to created_at for a
-- payment that never left `created`.
ALTER TABLE payments
    ADD COLUMN status_changed_at        timestamptz,
    ADD COLUMN reconcile_attempts       integer     NOT NULL DEFAULT 0,
    ADD COLUMN last_reconcile_attempt_at timestamptz,
    ADD COLUMN needs_manual_review      boolean     NOT NULL DEFAULT false;

UPDATE payments
SET status_changed_at = COALESCE(voided_at, failed_at, captured_at, authorized_at, created_at);

ALTER TABLE payments
    ALTER COLUMN status_changed_at SET NOT NULL,
    ALTER COLUMN status_changed_at SET DEFAULT now();

-- The reconciler's two hot scans: "which capturing/voiding rows have sat here
-- too long" and "which created/authorized rows have sat here too long with no
-- provider_payment_id update" both filter on (status, status_changed_at).
CREATE INDEX idx_payments_status_changed ON payments (status, status_changed_at);
-- Cheap "how many payments need a human" count for the alert this worker is
-- built to feed.
CREATE INDEX idx_payments_needs_review ON payments (needs_manual_review) WHERE needs_manual_review;

-- Same bookkeeping on payment_refunds: a refund stuck in_flight/pending needs
-- the identical lease clock and attempt guard.
ALTER TABLE payment_refunds
    ADD COLUMN status_changed_at         timestamptz,
    ADD COLUMN reconcile_attempts        integer     NOT NULL DEFAULT 0,
    ADD COLUMN last_reconcile_attempt_at timestamptz,
    ADD COLUMN needs_manual_review       boolean     NOT NULL DEFAULT false;

UPDATE payment_refunds
SET status_changed_at = COALESCE(updated_at, created_at);

ALTER TABLE payment_refunds
    ALTER COLUMN status_changed_at SET NOT NULL,
    ALTER COLUMN status_changed_at SET DEFAULT now();

CREATE INDEX idx_payment_refunds_status_changed ON payment_refunds (status, status_changed_at);
CREATE INDEX idx_payment_refunds_needs_review ON payment_refunds (needs_manual_review) WHERE needs_manual_review;

-- +goose Down
DROP INDEX idx_payment_refunds_needs_review;
DROP INDEX idx_payment_refunds_status_changed;
ALTER TABLE payment_refunds
    DROP COLUMN status_changed_at,
    DROP COLUMN reconcile_attempts,
    DROP COLUMN last_reconcile_attempt_at,
    DROP COLUMN needs_manual_review;

DROP INDEX idx_payments_needs_review;
DROP INDEX idx_payments_status_changed;
ALTER TABLE payments
    DROP COLUMN status_changed_at,
    DROP COLUMN reconcile_attempts,
    DROP COLUMN last_reconcile_attempt_at,
    DROP COLUMN needs_manual_review;
