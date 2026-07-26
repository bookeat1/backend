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
	// checkViolation is raised by chk_capacity_bucket_within_limit when a
	// table-less venue's declared capacity is already sold for the requested
	// window. Like the exclusion violation above it is a lost race, not a bug —
	// but ONLY for that one constraint: every other CHECK in this schema means
	// the application wrote something invalid, which must stay a 500.
	checkViolation = "23514"
	// capacityBucketConstraint is the constraint name from migration 0054.
	capacityBucketConstraint = "chk_capacity_bucket_within_limit"
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

// mapCapacityWrite is mapWrite plus the capacity-bucket CHECK: a write that
// would push a venue past its declared table-less capacity is a conflict (409),
// not a server error. The constraint is matched BY NAME so an unrelated CHECK
// failure keeps surfacing as the bug it is.
func mapCapacityWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		pgErr.Code == checkViolation && pgErr.ConstraintName == capacityBucketConstraint {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
	}
	return mapWrite(err, resource)
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
