// Package payment is the Postgres implementation of the payments domain
// repositories (internal/domain/payment*.go): payments, refunds, ledger
// entries, the transactional outbox, raw acquirer events, and the acquirer
// registry. Same layering and conventions as
// internal/infrastructure/postgres/booking — read that package first.
//
// The one rule that matters most here (team convention, "мы проверили перед
// вставкой" is forbidden): every status transition that can race a concurrent
// request is a single `UPDATE ... WHERE id = $1 AND <precondition>`, never a
// read followed by a write. A partial unique index
// (idx_payments_live_per_booking, idx_payments_idempotency,
// idx_payments_provider_payment, idx_payment_refunds_idempotency,
// idx_payment_refunds_provider, idx_payment_events_provider_event,
// idx_payment_providers_default) is the second, DB-enforced line of defence
// for the same invariant a CAS already tries to hold in Go — mapWrite turns
// any of them into domain.ErrAlreadyExists by inspecting the driver error's
// SQLSTATE code (23505) and, for diagnostics only, its ConstraintName. It
// never inspects err.Error() text: a client-message string is not something
// this package is allowed to depend on for a money-safety decision.
package payment

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
)

const (
	uniqueViolation = "23505"
)

// mapWrite maps a unique_violation to domain.ErrAlreadyExists, otherwise wraps
// err with resource for context. resource should name the entity/operation
// being written (e.g. "create payment"). The constraint name (when Postgres
// reports one) is included in the wrapped message purely as an operator-facing
// detail — the ERROR CLASS the caller acts on is always pgErr.Code, never any
// text.
func mapWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == uniqueViolation {
			if pgErr.ConstraintName != "" {
				return fmt.Errorf("%w: %s (constraint %s)", domain.ErrAlreadyExists, resource, pgErr.ConstraintName)
			}
			return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
		}
	}
	return fmt.Errorf("%s: %w", resource, err)
}

// window normalizes a limit coming straight from a caller (a reconciliation
// batch size, a claim limit) — same convention as booking.window.
func window(limit int) int {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return limit
}

const (
	defaultLimit = 50
	maxLimit     = 500
)

// page normalizes 1-based pagination into a LIMIT/OFFSET pair — same
// convention as booking.page.
func page(p, perPage int) (limit, offset int) {
	if p <= 0 {
		p = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return perPage, (p - 1) * perPage
}

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

type scanner interface{ Scan(dest ...any) error }
