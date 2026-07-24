// Package payout is the Postgres implementation of the restaurant-payout domain
// repositories (internal/domain/payout.go): payout destinations, payouts, the
// payout-item claim table, and the owed-balance reader over the payment ledger.
//
// It follows the exact conventions of internal/infrastructure/postgres/payment
// — read that package first. The one rule that matters most: every status
// transition that can race a concurrent request is a single
// `UPDATE ... WHERE id = $1 AND <precondition>`, never a read-then-write, and a
// UNIQUE index (uq_payouts_idempotency, uq_payout_items_ledger_entry) is the
// DB-enforced second line of defence. mapWrite turns a unique_violation into
// domain.ErrAlreadyExists by SQLSTATE code (23505), never by parsing the
// driver's message text.
package payout

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
)

const uniqueViolation = "23505"

// mapWrite maps a unique_violation to domain.ErrAlreadyExists, otherwise wraps
// err with resource for context. The constraint name (when Postgres reports
// one) is included purely as an operator-facing detail — the error CLASS the
// caller acts on is always pgErr.Code, never any message text.
func mapWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		if pgErr.ConstraintName != "" {
			return fmt.Errorf("%w: %s (constraint %s)", domain.ErrAlreadyExists, resource, pgErr.ConstraintName)
		}
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
	}
	return fmt.Errorf("%s: %w", resource, err)
}

const (
	defaultLimit = 50
	maxLimit     = 500
)

// window normalizes a caller-supplied limit — same convention as
// payment.window.
func window(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

type scanner interface{ Scan(dest ...any) error }
