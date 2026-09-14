// Package promocode is the Postgres implementation of
// domain.PromoCodeRepository: guest promo codes (migration 0108). The table
// carries NO activation counter on purpose — how much of a limit is spent is
// counted from the bookings themselves under the row lock LockByID takes
// (ADR-047).
package promocode

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// ErrNotInTransaction is returned by LockByID when it is called outside a
// transaction. It is a programming error, not a domain outcome: outside a
// transaction Postgres releases the row lock the moment the statement ends,
// so the caller would believe it serialized concurrent redemptions when it
// did not. Failing loudly beats handing out a limit twice.
var ErrNotInTransaction = errors.New("promo code lock requires an active transaction")

// Repository reads and writes promo_codes.
type Repository struct{ pool sqltx.Querier }

// New builds the repository over a pool or an active transaction.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PromoCodeRepository = (*Repository)(nil)

const cols = `id, code, promotion_id, starts_at, expires_at, max_uses_total,
	max_uses_per_user, status, created_by, created_at, updated_at`

// Create inserts a code. The id is generated here when the caller left it
// empty, like every other repository in this package tree. A duplicate code
// is ErrAlreadyExists (the unique index is the check — never a
// SELECT-then-INSERT, which two parallel admins would both pass), an unknown
// promotion_id is ErrNotFound, and a value the CHECK constraints refuse comes
// back as ErrValidation rather than a raw Postgres error.
func (r *Repository) Create(ctx context.Context, c *domain.PromoCode) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO promo_codes (id, code, promotion_id, starts_at, expires_at,
			max_uses_total, max_uses_per_user, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING created_at, updated_at`,
		c.ID, c.Code, c.PromotionID, c.StartsAt, c.ExpiresAt, c.MaxUsesTotal,
		c.MaxUsesPerUser, string(c.Status), c.CreatedBy).
		Scan(&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create promo code: %w", mapWriteError(err))
	}
	return nil
}

// GetByID reads a code by id regardless of status.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.PromoCode, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM promo_codes WHERE id = $1`, id)
	return scanCode(row, "get promo code")
}

// GetByCode reads a code by its NORMALIZED string. The caller normalizes
// (domain.NormalizePromoCode) — this is a plain equality lookup on the unique
// index, not a case-insensitive search, precisely because every spelling was
// already folded into one before it got here.
func (r *Repository) GetByCode(ctx context.Context, code string) (*domain.PromoCode, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM promo_codes WHERE code = $1`, code)
	return scanCode(row, "get promo code by code")
}

// LockByID reads the code and holds a row lock on it until the surrounding
// transaction ends.
//
// This is the whole concurrency control of the feature (ADR-047): every
// booking that carries the SAME code serializes here, so the activation count
// taken right after cannot be stale by the time the booking is inserted.
//
// LOCK ORDER — promo_code FIRST, venue SECOND. usecase/bookings takes this
// lock before capacity.LockVenue, and nothing may take them the other way
// round: two orders means a deadlock between two concurrent bookings.
//
// Outside a transaction this returns ErrNotInTransaction instead of silently
// taking a lock that is released immediately.
func (r *Repository) LockByID(ctx context.Context, id uuid.UUID) (*domain.PromoCode, error) {
	q := sqltx.From(ctx, r.pool)
	if _, ok := q.(pgx.Tx); !ok {
		return nil, fmt.Errorf("lock promo code: %w", ErrNotInTransaction)
	}
	row := q.QueryRow(ctx, `SELECT `+cols+` FROM promo_codes WHERE id = $1 FOR UPDATE`, id)
	return scanCode(row, "lock promo code")
}

// List returns codes matching filter, newest first — the admin listing's
// order.
func (r *Repository) List(ctx context.Context, filter domain.PromoCodeFilter) ([]domain.PromoCode, error) {
	args := make([]any, 0, 2)
	where := ""
	if filter.PromotionID != nil {
		args = append(args, *filter.PromotionID)
		where += fmt.Sprintf(" AND promotion_id = $%d", len(args))
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, s := range filter.Statuses {
			statuses = append(statuses, string(s))
		}
		args = append(args, statuses)
		where += fmt.Sprintf(" AND status = ANY($%d)", len(args))
	}

	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM promo_codes WHERE TRUE`+where+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list promo codes: %w", err)
	}
	defer rows.Close()

	out := make([]domain.PromoCode, 0)
	for rows.Next() {
		c, err := scanCode(rows, "list promo codes")
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list promo codes: %w", err)
	}
	return out, nil
}

// Update overwrites the mutable fields. The code string, created_by and
// created_at are NOT among them: the code a guest typed is already snapshotted
// on past bookings, so changing it would silently rewrite what those bookings
// mean. A zero-rows UPDATE means the id is absent.
func (r *Repository) Update(ctx context.Context, c *domain.PromoCode) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE promo_codes SET
			promotion_id = $2,
			starts_at = $3,
			expires_at = $4,
			max_uses_total = $5,
			max_uses_per_user = $6,
			status = $7,
			updated_at = now()
		 WHERE id = $1`,
		c.ID, c.PromotionID, c.StartsAt, c.ExpiresAt, c.MaxUsesTotal,
		c.MaxUsesPerUser, string(c.Status))
	if err != nil {
		return fmt.Errorf("update promo code: %w", mapWriteError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update promo code: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a code. Whether a redeemed code may be deleted at all is the
// usecase's call (it archives instead) — bookings hold no foreign key to this
// table, so Postgres would not stop it.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM promo_codes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete promo code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete promo code: %w", domain.ErrNotFound)
	}
	return nil
}

// mapWriteError turns the three Postgres failures this table can produce into
// the domain sentinels the transport layer already knows how to answer with.
func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case uniqueViolation:
		return domain.ErrAlreadyExists
	case foreignKeyViolation:
		// The only FK on this table is promotion_id → promos.
		return domain.ErrNotFound
	case checkViolation:
		// A window or a limit the CHECK constraints refuse. domain.PromoCode
		// .Validate catches these before the database does; reaching here
		// means a caller skipped it, and a 422 is still a better answer than
		// a 500.
		return domain.ErrValidation
	}
	return err
}

// scanRow is what both QueryRow and Rows satisfy.
type scanRow interface{ Scan(dest ...any) error }

func scanCode(row scanRow, op string) (*domain.PromoCode, error) {
	var c domain.PromoCode
	var status string
	err := row.Scan(&c.ID, &c.Code, &c.PromotionID, &c.StartsAt, &c.ExpiresAt,
		&c.MaxUsesTotal, &c.MaxUsesPerUser, &status, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	c.Status = domain.PromoCodeStatus(status)
	return &c, nil
}
