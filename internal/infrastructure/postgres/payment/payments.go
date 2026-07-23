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

// Repository implements domain.PaymentRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the payment repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PaymentRepository = (*Repository)(nil)

const paymentCols = `id, booking_id, restaurant_id, user_id, provider, provider_payment_id,
	purpose, status, amount_minor, base_amount_minor, fee_minor, currency,
	idempotency_key, payment_url, authorized_at, captured_at, voided_at, failed_at,
	expires_at, failure_code, failure_message,
	settled_at, settled_trigger, settlement_idempotency_key,
	status_changed_at, reconcile_attempts, last_reconcile_attempt_at, needs_manual_review,
	created_at, updated_at`

// liveStatuses mirrors domain.PaymentStatus.HoldsMoney and
// idx_payments_live_per_booking exactly — keep all three in sync (the
// migration's own comment carries the same warning).
var liveStatuses = []string{
	string(domain.PaymentAuthorized), string(domain.PaymentCapturing),
	string(domain.PaymentVoiding), string(domain.PaymentCaptured),
}

// settleableStatuses mirrors domain.PaymentStatus.SettleResolvable exactly:
// liveStatuses plus the two outcomes a completed Settle call can leave a
// payment in (refunded / partially_refunded). Keep both lists and the domain
// predicate in sync — see GetSettleableByBookingID.
var settleableStatuses = append(append([]string{}, liveStatuses...),
	string(domain.PaymentRefunded), string(domain.PaymentPartiallyRefunded))

func (r *Repository) Create(ctx context.Context, p *domain.Payment) error {
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.StatusChangedAt.IsZero() {
		p.StatusChangedAt = p.CreatedAt
	}
	q := `INSERT INTO payments (` + paymentCols + `) VALUES (
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
		$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(p)...); err != nil {
		return mapWrite(err, "create payment")
	}
	return nil
}

// Update rewrites every mutable column. Not used by the current usecase layer
// (which prefers CompareAndSwapStatus / ClaimSettlement for anything that
// matters to the race guard), but implemented in full — same convention as
// booking.Repository.Update — for whatever future admin-facing correction
// tool needs it.
func (r *Repository) Update(ctx context.Context, p *domain.Payment) error {
	p.UpdatedAt = time.Now()
	q := `UPDATE payments SET
		user_id=$2, provider_payment_id=$3, purpose=$4, status=$5,
		amount_minor=$6, base_amount_minor=$7, fee_minor=$8, currency=$9,
		idempotency_key=$10, payment_url=$11, authorized_at=$12, captured_at=$13,
		voided_at=$14, failed_at=$15, expires_at=$16, failure_code=$17, failure_message=$18,
		settled_at=$19, settled_trigger=$20, settlement_idempotency_key=$21,
		status_changed_at=$22, reconcile_attempts=$23, last_reconcile_attempt_at=$24,
		needs_manual_review=$25, updated_at=$26
		WHERE id=$1`
	args := []any{
		p.ID, p.UserID, p.ProviderPaymentID, string(p.Purpose), string(p.Status),
		p.AmountMinor, p.BaseAmountMinor, p.FeeMinor, string(p.Currency),
		p.IdempotencyKey, p.PaymentURL, p.AuthorizedAt, p.CapturedAt,
		p.VoidedAt, p.FailedAt, p.ExpiresAt, p.FailureCode, p.FailureMessage,
		p.SettledAt, triggerToDB(p.SettledTrigger), p.SettlementIdempotencyKey,
		p.StatusChangedAt, p.ReconcileAttempts, p.LastReconcileAttemptAt,
		p.NeedsManualReview, p.UpdatedAt,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update payment")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id=$1`, id)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment: %w", err)
	}
	return p, nil
}

func (r *Repository) GetByProviderPaymentID(ctx context.Context, provider domain.PaymentProvider, providerPaymentID string) (*domain.Payment, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+paymentCols+` FROM payments WHERE provider=$1 AND provider_payment_id=$2`,
		string(provider), providerPaymentID)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment by provider payment id: %w", err)
	}
	return p, nil
}

// GetLiveByBookingID relies on idx_payments_live_per_booking to guarantee at
// most one matching row exists — no ORDER BY / LIMIT tie-break is needed.
func (r *Repository) GetLiveByBookingID(ctx context.Context, bookingID uuid.UUID) (*domain.Payment, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+paymentCols+` FROM payments WHERE booking_id=$1 AND status = ANY($2)`,
		bookingID, liveStatuses)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get live payment for booking: %w", err)
	}
	return p, nil
}

// GetSettleableByBookingID returns the booking's payment across the wider
// settleableStatuses set (see that var's doc) — unlike GetLiveByBookingID,
// there is no unique index guaranteeing at most one matching row here (only
// the live subset is covered by idx_payments_live_per_booking), so this
// orders by created_at DESC and takes the most recent one. In practice a
// booking never accumulates more than one settleable payment (CreateForBooking
// refuses to authorize a new payment once the booking has left
// pending/confirmed, which is exactly the state a settleable payment implies),
// but the ORDER BY/LIMIT makes the result deterministic instead of an
// arbitrary row if that invariant is ever violated.
func (r *Repository) GetSettleableByBookingID(ctx context.Context, bookingID uuid.UUID) (*domain.Payment, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+paymentCols+` FROM payments WHERE booking_id=$1 AND status = ANY($2)
		 ORDER BY created_at DESC LIMIT 1`,
		bookingID, settleableStatuses)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get settleable payment for booking: %w", err)
	}
	return p, nil
}

func (r *Repository) GetByIdempotencyKey(ctx context.Context, provider domain.PaymentProvider, idempotencyKey string) (*domain.Payment, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+paymentCols+` FROM payments WHERE provider=$1 AND idempotency_key=$2`,
		string(provider), idempotencyKey)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment by idempotency key: %w", err)
	}
	return p, nil
}

func (r *Repository) List(ctx context.Context, f domain.PaymentFilter) ([]domain.Payment, int, error) {
	where := []string{"true"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.BookingID != nil {
		add("booking_id = $%d", *f.BookingID)
	}
	if f.RestaurantID != nil {
		add("restaurant_id = $%d", *f.RestaurantID)
	}
	if f.UserID != nil {
		add("user_id = $%d", *f.UserID)
	}
	if len(f.Statuses) > 0 {
		add("status = ANY($%d)", statusStrings(f.Statuses))
	}
	if f.From != nil {
		add("created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("created_at < $%d", *f.To)
	}
	whereSQL := joinAND(where)

	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM payments WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count payments: %w", err)
	}

	limit, offset := page(f.Page, f.PerPage)
	args = append(args, limit, offset)
	q := `SELECT ` + paymentCols + ` FROM payments WHERE ` + whereSQL + `
		ORDER BY created_at DESC, id
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list payments: %w", err)
	}
	defer rows.Close()
	out, err := scanPayments(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("list payments: %w", err)
	}
	return out, total, nil
}

// UpdateStatus is a blind write (no precondition on the current status): the
// caller has already decided the transition is safe without racing anyone
// (or is fine losing a race silently, e.g. an admin correction tool). Prefer
// CompareAndSwapStatus for anything a concurrent request could also be
// changing. It stamps the lifecycle timestamp column that belongs to the new
// status and resets the reconciliation lease/attempt bookkeeping — the same
// contract CompareAndSwapStatus keeps, see its doc comment below.
func (r *Repository) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PaymentStatus, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, statusWriteSQL+` WHERE id=$1`,
		id, string(status), at)
	if err != nil {
		return mapWrite(err, "update payment status")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// statusWriteSQL is shared by UpdateStatus and CompareAndSwapStatus: both
// write the SAME set of columns on a real status change, they only differ in
// their WHERE clause. The CASE WHEN branches are driven entirely by the bound
// $2 parameter, never by string interpolation, so this stays injection-safe
// while still touching only the ONE lifecycle column that belongs to the new
// status. $2 is cast to varchar explicitly on its first use — pgx prepares
// this as an extended-protocol statement, and Postgres otherwise deduces two
// different types for the SAME parameter number from `status = $2` (varchar,
// from the column) versus `$2 = 'authorized'` (unknown/text, from the bare
// literal), which it rejects as SQLSTATE 42P08. One explicit cast fixes the
// parameter's type for every later bare use of $2 in the same statement.
const statusWriteSQL = `UPDATE payments SET
	status = $2::varchar,
	updated_at = $3,
	status_changed_at = $3,
	reconcile_attempts = 0,
	last_reconcile_attempt_at = NULL,
	needs_manual_review = false,
	authorized_at = CASE WHEN $2 = 'authorized' THEN $3 ELSE authorized_at END,
	captured_at   = CASE WHEN $2 = 'captured'   THEN $3 ELSE captured_at   END,
	voided_at     = CASE WHEN $2 = 'voided'     THEN $3 ELSE voided_at     END,
	failed_at     = CASE WHEN $2 = 'failed'     THEN $3 ELSE failed_at     END`

// CompareAndSwapStatus is the database-level guard the whole payments layer
// depends on: ONE statement, `UPDATE ... WHERE id = $1 AND status = $2`,
// never a read followed by a write. Two things can make it fail, and both
// surface as domain.ErrAlreadyExists (mapWrite for the first, the
// zero-rows-affected branch for the second):
//
//   - a concurrent transition into a "live" status
//     (authorized/capturing/voiding/captured) for the SAME booking collides
//     with idx_payments_live_per_booking — a genuine unique_violation from
//     Postgres, caught by mapWrite;
//   - the row's current status is not `from` any more (a concurrent
//     transition already moved it, or the id does not exist at all) — no
//     Postgres error, just zero rows affected. classifyCASMiss tells these
//     two apart the same way fakePaymentRepo.CompareAndSwapStatus does:
//     ErrNotFound when the id itself is unknown, ErrAlreadyExists when the id
//     exists but was not in the expected `from` status.
func (r *Repository) CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to domain.PaymentStatus, at time.Time) error {
	const q = statusWriteSQL + ` WHERE id=$1 AND status=$4`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, id, string(to), at, string(from))
	if err != nil {
		return mapWrite(err, "compare-and-swap payment status")
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id)
	}
	return nil
}

// classifyMiss is called after a CAS-style UPDATE affected zero rows. It
// makes exactly one extra read — after the write already failed, not before
// a decision to write — solely to report ErrNotFound vs ErrAlreadyExists
// accurately; it introduces no new race because no further write follows it.
func (r *Repository) classifyMiss(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payments WHERE id=$1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check payment existence: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return domain.ErrAlreadyExists
}

// ClaimSettlement is the CAS anchor behind Payment.SettledAt: one
// `UPDATE ... WHERE id = $1 AND settled_at IS NULL`. Zero rows affected means
// either the id does not exist (ErrNotFound) or it was already settled
// (ErrAlreadyExists) — classifyMiss tells them apart the same way as
// CompareAndSwapStatus.
func (r *Repository) ClaimSettlement(ctx context.Context, id uuid.UUID, idempotencyKey string, trigger domain.RefundTrigger, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payments SET settled_at=$2, settled_trigger=$3, settlement_idempotency_key=$4
		 WHERE id=$1 AND settled_at IS NULL`,
		id, at, string(trigger), idempotencyKey)
	if err != nil {
		return mapWrite(err, "claim payment settlement")
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id)
	}
	return nil
}

// ClaimStale must run inside a TxManager transaction — FOR UPDATE SKIP LOCKED
// releases its locks the instant the transaction ends, so calling it outside
// one lets two reconciler passes claim the same row. The lock itself is never
// held across the acquirer call the reconciler makes next (see
// domain.PaymentRepository.ClaimStale's doc comment) — this method only
// selects and locks, the caller's own CAS writes are what actually decide who
// wins.
func (r *Repository) ClaimStale(ctx context.Context, statuses []domain.PaymentStatus, before time.Time, limit int) ([]domain.Payment, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+paymentCols+` FROM payments
		 WHERE status = ANY($1) AND status_changed_at < $2
		 ORDER BY status_changed_at, id
		 LIMIT $3
		 FOR UPDATE SKIP LOCKED`,
		statusStrings(statuses), before, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim stale payments: %w", err)
	}
	defer rows.Close()
	out, err := scanPayments(rows)
	if err != nil {
		return nil, fmt.Errorf("claim stale payments: %w", err)
	}
	return out, nil
}

// ClaimExpiredHolds backs idx_payments_expires. Same locking caveat as
// ClaimStale.
func (r *Repository) ClaimExpiredHolds(ctx context.Context, before time.Time, limit int) ([]domain.Payment, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+paymentCols+` FROM payments
		 WHERE status = 'authorized' AND expires_at IS NOT NULL AND expires_at < $1
		 ORDER BY expires_at, id
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		before, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim expired holds: %w", err)
	}
	defer rows.Close()
	out, err := scanPayments(rows)
	if err != nil {
		return nil, fmt.Errorf("claim expired holds: %w", err)
	}
	return out, nil
}

// RecordReconcileAttempt is CAS-guarded the same way as CompareAndSwapStatus:
// `UPDATE ... WHERE id = $1 AND status = $2 RETURNING ...`. Zero rows means
// the row's status already moved on (or the id is unknown) between the
// worker's read and this write — classifyMiss reports which.
func (r *Repository) RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus domain.PaymentStatus, at time.Time, maxAttempts int) (int, bool, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE payments SET
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
		return 0, false, fmt.Errorf("record reconcile attempt: %w", err)
	}
	return attempts, needsReview, nil
}

func (r *Repository) args(p *domain.Payment) []any {
	return []any{
		p.ID, p.BookingID, p.RestaurantID, p.UserID, string(p.Provider), p.ProviderPaymentID,
		string(p.Purpose), string(p.Status), p.AmountMinor, p.BaseAmountMinor, p.FeeMinor, string(p.Currency),
		p.IdempotencyKey, p.PaymentURL, p.AuthorizedAt, p.CapturedAt, p.VoidedAt, p.FailedAt,
		p.ExpiresAt, p.FailureCode, p.FailureMessage,
		p.SettledAt, triggerToDB(p.SettledTrigger), p.SettlementIdempotencyKey,
		p.StatusChangedAt, p.ReconcileAttempts, p.LastReconcileAttemptAt, p.NeedsManualReview,
		p.CreatedAt, p.UpdatedAt,
	}
}

func scanPayments(rows pgx.Rows) ([]domain.Payment, error) {
	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func scanPayment(row scanner) (*domain.Payment, error) {
	var p domain.Payment
	var provider, purpose, status, currency string
	var settledTrigger *string
	if err := row.Scan(
		&p.ID, &p.BookingID, &p.RestaurantID, &p.UserID, &provider, &p.ProviderPaymentID,
		&purpose, &status, &p.AmountMinor, &p.BaseAmountMinor, &p.FeeMinor, &currency,
		&p.IdempotencyKey, &p.PaymentURL, &p.AuthorizedAt, &p.CapturedAt, &p.VoidedAt, &p.FailedAt,
		&p.ExpiresAt, &p.FailureCode, &p.FailureMessage,
		&p.SettledAt, &settledTrigger, &p.SettlementIdempotencyKey,
		&p.StatusChangedAt, &p.ReconcileAttempts, &p.LastReconcileAttemptAt, &p.NeedsManualReview,
		&p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.Provider = domain.PaymentProvider(provider)
	p.Purpose = domain.PaymentPurpose(purpose)
	p.Status = domain.PaymentStatus(status)
	p.Currency = domain.Currency(currency)
	if settledTrigger != nil {
		t := domain.RefundTrigger(*settledTrigger)
		p.SettledTrigger = &t
	}
	return &p, nil
}

func statusStrings[T ~string](ss []T) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

func triggerToDB(t *domain.RefundTrigger) any {
	if t == nil {
		return nil
	}
	return string(*t)
}

func joinAND(clauses []string) string {
	out := clauses[0]
	for _, c := range clauses[1:] {
		out += " AND " + c
	}
	return out
}
