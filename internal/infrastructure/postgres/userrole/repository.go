// Package userrole persists global roles and the audit of their changes.
package userrole

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.UserRoleRepository.
type Repository struct {
	pool sqltx.Querier
	tx   domain.TxManager
}

// New builds the role repository. The transaction manager is required: writing
// the role without its audit row, or the other way round, is exactly the state
// this feature exists to prevent.
func New(pool sqltx.Querier, tx domain.TxManager) *Repository {
	return &Repository{pool: pool, tx: tx}
}

var _ domain.UserRoleRepository = (*Repository)(nil)

// SetRole writes the new role and its audit entry atomically.
func (r *Repository) SetRole(ctx context.Context, c domain.UserRoleChange) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := sqltx.From(ctx, r.pool)
		// from_role is re-read in the UPDATE's WHERE rather than trusted from the
		// caller: two administrators pressing at the same moment must not both
		// believe they moved the user from the same starting role.
		tag, err := q.Exec(ctx,
			`UPDATE users SET role = $2, updated_at = now()
			  WHERE id = $1 AND role = $3`, c.UserID, string(c.ToRole), string(c.FromRole))
		if err != nil {
			return fmt.Errorf("set role: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Either the user is gone, or somebody changed the role first. Both
			// mean "your view was stale", and both must not leave an audit row
			// describing a change that did not happen.
			return fmt.Errorf("%w: user role changed concurrently", domain.ErrInvalidStatus)
		}
		_, err = q.Exec(ctx,
			`INSERT INTO user_role_changes (user_id, actor_id, from_role, to_role, reason)
			 VALUES ($1,$2,$3,$4,$5)`,
			c.UserID, c.ActorID, string(c.FromRole), string(c.ToRole), c.Reason)
		if err != nil {
			return fmt.Errorf("record role change: %w", err)
		}
		return nil
	})
}

// CountByRole counts users currently holding a role. Deleted accounts are
// excluded: a soft-deleted administrator cannot sign in, so counting them would
// let the platform end up with no usable admin while the check says otherwise.
func (r *Repository) CountByRole(ctx context.Context, role domain.Role) (int, error) {
	var n int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = $1 AND deleted_at IS NULL`, string(role)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count role: %w", err)
	}
	return n, nil
}

// History returns a user's role changes, newest first.
func (r *Repository) History(ctx context.Context, userID uuid.UUID, limit int) ([]domain.UserRoleChange, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, user_id, actor_id, from_role, to_role, reason, created_at
		   FROM user_role_changes
		  WHERE user_id = $1
		  ORDER BY created_at DESC
		  LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("role history: %w", err)
	}
	defer rows.Close()

	out := []domain.UserRoleChange{}
	for rows.Next() {
		var c domain.UserRoleChange
		if err := rows.Scan(&c.ID, &c.UserID, &c.ActorID, &c.FromRole, &c.ToRole, &c.Reason, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan role change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Search finds users by a fragment of email, phone or name.
//
// An empty query lists the most recent accounts rather than every user ever
// created: the screen behind this is "find the person I want to promote", and
// an unbounded dump of the whole user table is neither useful nor cheap.
func (r *Repository) Search(ctx context.Context, query string, limit int) ([]domain.User, error) {
	const cols = `id, email, phone, full_name, role, is_active, created_at`
	var rows pgx.Rows
	var err error
	if query == "" {
		rows, err = sqltx.From(ctx, r.pool).Query(ctx,
			`SELECT `+cols+` FROM users WHERE deleted_at IS NULL
			  ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = sqltx.From(ctx, r.pool).Query(ctx,
			`SELECT `+cols+` FROM users
			  WHERE deleted_at IS NULL
			    AND (email ILIKE '%'||$1||'%' OR phone ILIKE '%'||$1||'%' OR full_name ILIKE '%'||$1||'%')
			  ORDER BY created_at DESC LIMIT $2`, query, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()

	out := []domain.User{}
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Email, &u.Phone, &u.FullName, &u.Role, &u.IsActive, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return []domain.User{}, nil
	}
	return out, nil
}
