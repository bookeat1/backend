package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Refunds implements domain.PaymentRefundRepository.
type Refunds struct{ pool sqltx.Querier }

// NewRefunds builds the refund repository.
func NewRefunds(pool sqltx.Querier) *Refunds { return &Refunds{pool: pool} }

var _ domain.PaymentRefundRepository = (*Refunds)(nil)

const refundCols = `id, payment_id, provider_refund_id, amount_minor, currency, status,
	reason, idempotency_key, failure_code, failure_message,
	status_changed_at, reconcile_attempts, last_reconcile_attempt_at, needs_manual_review,
	created_at, updated_at`

func (r *Refunds) Create(ctx context.Context, rf *domain.PaymentRefund) error {
	now := time.Now()
	if rf.CreatedAt.IsZero() {
		rf.CreatedAt = now
	}
	rf.UpdatedAt = now
	if rf.StatusChangedAt.IsZero() {
		rf.StatusChangedAt = rf.CreatedAt
	}
	q := `INSERT INTO payment_refunds (` + refundCols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(rf)...); err != nil {
		return mapWrite(err, "create payment refund")
	}
	return nil
}

// Update rewrites every mutable column with a blind write, no status
// precondition — this is the write claimAndCallGateway uses AFTER it has
// already won the created→in_flight CAS below, to persist the acquirer's
// final answer (succeeded/failed/pending) on a row only it is allowed to
// touch right now. It never resets status_changed_at/reconcile bookkeeping on
// its own (same contract as fakeRefundRepo.Update) — only
// CompareAndSwapStatus does that, deliberately, so a caller that wants the
// CAS reset semantics must go through it.
func (r *Refunds) Update(ctx context.Context, rf *domain.PaymentRefund) error {
	q := `UPDATE payment_refunds SET
		provider_refund_id=$2, amount_minor=$3, currency=$4, status=$5,
		reason=$6, idempotency_key=$7, failure_code=$8, failure_message=$9,
		status_changed_at=$10, reconcile_attempts=$11, last_reconcile_attempt_at=$12,
		needs_manual_review=$13, updated_at=$14
		WHERE id=$1`
	args := []any{
		rf.ID, rf.ProviderRefundID, rf.AmountMinor, string(rf.Currency), string(rf.Status),
		rf.Reason, rf.IdempotencyKey, rf.FailureCode, rf.FailureMessage,
		rf.StatusChangedAt, rf.ReconcileAttempts, rf.LastReconcileAttemptAt,
		rf.NeedsManualReview, rf.UpdatedAt,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update payment refund")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Refunds) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentRefund, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+refundCols+` FROM payment_refunds WHERE id=$1`, id)
	rf, err := scanRefund(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment refund: %w", err)
	}
	return rf, nil
}

func (r *Refunds) GetByIdempotencyKey(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.PaymentRefund, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+refundCols+` FROM payment_refunds WHERE payment_id=$1 AND idempotency_key=$2`,
		paymentID, idempotencyKey)
	rf, err := scanRefund(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment refund by idempotency key: %w", err)
	}
	return rf, nil
}

func (r *Refunds) ListByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]domain.PaymentRefund, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+refundCols+` FROM payment_refunds WHERE payment_id=$1 ORDER BY created_at, id`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("list payment refunds: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentRefund
	for rows.Next() {
		rf, err := scanRefund(rows)
		if err != nil {
			return nil, fmt.Errorf("list payment refunds: %w", err)
		}
		out = append(out, *rf)
	}
	return out, rows.Err()
}

// SucceededTotal is the left-hand side of "never refund more than what is
// left" (spec §8) — must be read by the caller before the next refund attempt
// is authorized.
func (r *Refunds) SucceededTotal(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	var total int64
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_minor), 0) FROM payment_refunds WHERE payment_id=$1 AND status=$2`,
		paymentID, string(domain.RefundSucceeded)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("sum succeeded refunds: %w", err)
	}
	return total, nil
}

// CompareAndSwapStatus is the same DB-level guard as
// Repository.CompareAndSwapStatus, applied to one refund row: it is what
// makes the created→in_flight claim exclusive (report item #2) — the loser
// of a concurrent Settle race for the same idempotency key must never reach
// the acquirer call.
func (r *Refunds) CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to domain.RefundStatus, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payment_refunds SET
			status=$2, updated_at=$3, status_changed_at=$3,
			reconcile_attempts=0, last_reconcile_attempt_at=NULL, needs_manual_review=false
		 WHERE id=$1 AND status=$4`,
		id, string(to), at, string(from))
	if err != nil {
		return mapWrite(err, "compare-and-swap refund status")
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id)
	}
	return nil
}

func (r *Refunds) classifyMiss(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_refunds WHERE id=$1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check payment refund existence: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return domain.ErrAlreadyExists
}

// ClaimStale must run inside a TxManager transaction, same locking caveat as
// Repository.ClaimStale.
func (r *Refunds) ClaimStale(ctx context.Context, statuses []domain.RefundStatus, before time.Time, limit int) ([]domain.PaymentRefund, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+refundCols+` FROM payment_refunds
		 WHERE status = ANY($1) AND status_changed_at < $2
		 ORDER BY status_changed_at, id
		 LIMIT $3
		 FOR UPDATE SKIP LOCKED`,
		statusStrings(statuses), before, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim stale refunds: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentRefund
	for rows.Next() {
		rf, err := scanRefund(rows)
		if err != nil {
			return nil, fmt.Errorf("claim stale refunds: %w", err)
		}
		out = append(out, *rf)
	}
	return out, rows.Err()
}

// RecordReconcileAttempt mirrors Repository.RecordReconcileAttempt exactly,
// applied to payment_refunds.
func (r *Refunds) RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus domain.RefundStatus, at time.Time, maxAttempts int) (int, bool, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE payment_refunds SET
			reconcile_attempts = reconcile_attempts + 1,
			last_reconcile_attempt_at = $3,
			needs_manual_review = (reconcile_attempts + 1 >= $4)
		 WHERE id = $1 AND status = $2
		 RETURNING reconcile_attempts, needs_manual_review`,
		id, string(expectedStatus), at, maxAttempts)
	var attempts int
	var needsReview bool
	if err := row.Scan(&attempts, &needsReview); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, r.classifyMiss(ctx, id)
		}
		return 0, false, fmt.Errorf("record refund reconcile attempt: %w", err)
	}
	return attempts, needsReview, nil
}

func (r *Refunds) args(rf *domain.PaymentRefund) []any {
	return []any{
		rf.ID, rf.PaymentID, rf.ProviderRefundID, rf.AmountMinor, string(rf.Currency), string(rf.Status),
		rf.Reason, rf.IdempotencyKey, rf.FailureCode, rf.FailureMessage,
		rf.StatusChangedAt, rf.ReconcileAttempts, rf.LastReconcileAttemptAt, rf.NeedsManualReview,
		rf.CreatedAt, rf.UpdatedAt,
	}
}

func scanRefund(row scanner) (*domain.PaymentRefund, error) {
	var rf domain.PaymentRefund
	var currency, status string
	if err := row.Scan(
		&rf.ID, &rf.PaymentID, &rf.ProviderRefundID, &rf.AmountMinor, &currency, &status,
		&rf.Reason, &rf.IdempotencyKey, &rf.FailureCode, &rf.FailureMessage,
		&rf.StatusChangedAt, &rf.ReconcileAttempts, &rf.LastReconcileAttemptAt, &rf.NeedsManualReview,
		&rf.CreatedAt, &rf.UpdatedAt,
	); err != nil {
		return nil, err
	}
	rf.Currency = domain.Currency(currency)
	rf.Status = domain.RefundStatus(status)
	return &rf, nil
}
