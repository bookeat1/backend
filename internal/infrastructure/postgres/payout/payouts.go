package payout

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

// Payouts implements domain.PayoutRepository.
type Payouts struct{ pool sqltx.Querier }

// NewPayouts builds the payout repository.
func NewPayouts(pool sqltx.Querier) *Payouts { return &Payouts{pool: pool} }

var _ domain.PayoutRepository = (*Payouts)(nil)

const payoutCols = `id, restaurant_id, amount_minor, currency, status, method, destination_token,
	destination_customer_ref, provider_ref, idempotency_key, failure_code, failure_reason,
	status_changed_at, reconcile_attempts, last_reconcile_attempt_at, needs_manual_review,
	sent_at, paid_at, failed_at, created_at, updated_at`

// Create inserts a new payout. A duplicate idempotency_key surfaces as
// domain.ErrAlreadyExists (uq_payouts_idempotency).
func (r *Payouts) Create(ctx context.Context, p *domain.Payout) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.StatusChangedAt.IsZero() {
		p.StatusChangedAt = now
	}
	p.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO payouts (`+payoutCols+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		p.ID, p.RestaurantID, p.AmountMinor, string(p.Currency), string(p.Status), string(p.Method),
		p.DestinationToken, p.DestinationCustomerRef, p.ProviderRef, p.IdempotencyKey, p.FailureCode,
		p.FailureReason, p.StatusChangedAt, p.ReconcileAttempts, p.LastReconcileAttemptAt,
		p.NeedsManualReview, p.SentAt, p.PaidAt, p.FailedAt, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return mapWrite(err, "create payout")
	}
	return nil
}

// GetByID returns a payout or domain.ErrNotFound.
func (r *Payouts) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payout, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts WHERE id=$1`, id)
	return scanOnePayout(row)
}

// GetByIdempotencyKey resolves our own retry token.
func (r *Payouts) GetByIdempotencyKey(ctx context.Context, key string) (*domain.Payout, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+payoutCols+` FROM payouts WHERE idempotency_key=$1`, key)
	return scanOnePayout(row)
}

// statusWriteSQL sets the new status, stamps status_changed_at, resets the
// reconcile counter on any real transition, and fills the per-transition
// timestamp + optional patch columns. The COALESCE keeps a column untouched
// when the caller passes nil for it.
const statusWriteSQL = `UPDATE payouts SET
	status = $2::varchar,
	status_changed_at = $3,
	updated_at = $3,
	reconcile_attempts = 0,
	needs_manual_review = false,
	provider_ref   = COALESCE($4, provider_ref),
	failure_code   = COALESCE($5, failure_code),
	failure_reason = COALESCE($6, failure_reason),
	sent_at   = CASE WHEN $2::varchar = 'sent'   THEN $3 ELSE sent_at   END,
	paid_at   = CASE WHEN $2::varchar = 'paid'   THEN $3 ELSE paid_at   END,
	failed_at = CASE WHEN $2::varchar = 'failed' THEN $3 ELSE failed_at END`

// CompareAndSwapStatus is the database-level guard: ONE
// `UPDATE ... WHERE id=$1 AND status=$7`. Zero rows means the row moved on (or
// the id is unknown) — classifyMiss reports ErrNotFound vs ErrAlreadyExists.
// The $2 status parameter is cast ::varchar on first use to avoid pgx's
// extended-protocol SQLSTATE 42P08 (a bound param reused as a column value and
// inside a CASE comparison deduces two types) — the same gotcha the payments
// layer documents.
func (r *Payouts) CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to domain.PayoutStatus, patch domain.PayoutStatusPatch, at time.Time) error {
	const q = statusWriteSQL + ` WHERE id=$1 AND status=$7`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		id, string(to), at, patch.ProviderRef, patch.FailureCode, patch.FailureReason, string(from))
	if err != nil {
		return mapWrite(err, "compare-and-swap payout status")
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id)
	}
	return nil
}

// SetProviderRef records the acquirer payout id on a still-`sent` payout
// without a status change. Idempotent via COALESCE: an existing ref is never
// overwritten. A no-op (payout already moved on, or ref already set) is not an
// error — the reconciler still has our order id to query by.
func (r *Payouts) SetProviderRef(ctx context.Context, id uuid.UUID, providerRef string) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payouts SET provider_ref = COALESCE(provider_ref, $2), updated_at = now()
		 WHERE id = $1 AND status = 'sent'`,
		id, providerRef); err != nil {
		return fmt.Errorf("set payout provider ref: %w", err)
	}
	return nil
}

// ClaimStale must run inside a TxManager transaction so FOR UPDATE SKIP LOCKED
// actually holds its lock (see domain.PayoutRepository.ClaimStale). The lock is
// never held across the acquirer call the reconciler makes next; correctness
// comes from the CAS writes afterwards, not from this lock.
func (r *Payouts) ClaimStale(ctx context.Context, statuses []domain.PayoutStatus, before time.Time, limit int) ([]domain.Payout, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+payoutCols+` FROM payouts
		 WHERE status = ANY($1) AND status_changed_at < $2
		 ORDER BY status_changed_at, id
		 LIMIT $3
		 FOR UPDATE SKIP LOCKED`,
		statusStrings(statuses), before, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim stale payouts: %w", err)
	}
	defer rows.Close()
	return scanPayouts(rows)
}

// RecordReconcileAttempt is CAS-guarded the same way as CompareAndSwapStatus.
func (r *Payouts) RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus domain.PayoutStatus, at time.Time, maxAttempts int) (int, bool, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE payouts SET
			reconcile_attempts = reconcile_attempts + 1,
			last_reconcile_attempt_at = $3,
			updated_at = $3,
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
		return 0, false, fmt.Errorf("record payout reconcile attempt: %w", err)
	}
	return attempts, needsReview, nil
}

// List returns a restaurant's payouts, newest first.
func (r *Payouts) List(ctx context.Context, restaurantID uuid.UUID, limit int) ([]domain.Payout, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+payoutCols+` FROM payouts WHERE restaurant_id=$1
		 ORDER BY created_at DESC, id DESC LIMIT $2`,
		restaurantID, window(limit))
	if err != nil {
		return nil, fmt.Errorf("list payouts: %w", err)
	}
	defer rows.Close()
	return scanPayouts(rows)
}

func (r *Payouts) classifyMiss(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payouts WHERE id=$1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check payout existence: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return domain.ErrAlreadyExists
}

func statusStrings(statuses []domain.PayoutStatus) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = string(s)
	}
	return out
}

func scanOnePayout(row scanner) (*domain.Payout, error) {
	p, err := scanPayout(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan payout: %w", err)
	}
	return p, nil
}

func scanPayout(row scanner) (*domain.Payout, error) {
	var p domain.Payout
	var currency, status, method string
	if err := row.Scan(&p.ID, &p.RestaurantID, &p.AmountMinor, &currency, &status, &method,
		&p.DestinationToken, &p.DestinationCustomerRef, &p.ProviderRef, &p.IdempotencyKey, &p.FailureCode,
		&p.FailureReason, &p.StatusChangedAt, &p.ReconcileAttempts, &p.LastReconcileAttemptAt,
		&p.NeedsManualReview, &p.SentAt, &p.PaidAt, &p.FailedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Currency = domain.Currency(currency)
	p.Status = domain.PayoutStatus(status)
	p.Method = domain.PayoutMethod(method)
	return &p, nil
}

func scanPayouts(rows pgx.Rows) ([]domain.Payout, error) {
	var out []domain.Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, fmt.Errorf("scan payouts: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
