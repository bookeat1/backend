// Package booking is the Postgres implementation of the booking repositories.
package booking

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
)

const (
	uniqueViolation = "23505"
	// exclusionViolation is raised by the GiST EXCLUDE constraint on
	// booking_tables when two active slots for the same table overlap. It is a
	// lost race for a table, i.e. a conflict — mapping it anywhere else would
	// turn a legitimate 409 into a 500.
	exclusionViolation = "23P01"
)

// mapWrite maps a unique_violation or an exclusion_violation to
// domain.ErrAlreadyExists, otherwise wraps err with resource for context.
// resource should name the entity/operation being written (e.g. "create
// booking").
func mapWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case uniqueViolation, exclusionViolation:
			return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
		}
	}
	return fmt.Errorf("%s: %w", resource, err)
}

// page normalizes 1-based pagination into a LIMIT/OFFSET pair.
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

// window normalizes a limit/offset pair coming straight from a caller.
func window(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultPerPage
	}
	if limit > maxPerPage {
		limit = maxPerPage
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

type scanner interface{ Scan(dest ...any) error }
