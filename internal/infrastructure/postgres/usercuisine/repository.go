// Package usercuisine is the Postgres implementation of
// domain.UserCuisinePreferenceRepository (foodie-profile cuisine picks,
// many-to-many against the existing restaurant_categories dictionary).
package usercuisine

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.UserCuisinePreferenceRepository = (*Repository)(nil)

// foreignKeyViolation is the Postgres SQLSTATE for a foreign_key_violation.
const foreignKeyViolation = "23503"

func (r *Repository) ListCategoryIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT category_id FROM user_cuisine_preferences WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("list cuisine preferences: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list cuisine preferences: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cuisine preferences: %w", err)
	}
	return ids, nil
}

// Replace deletes the user's existing preferences and inserts categoryIDs.
// The delete and each insert are separate statements, not atomic on their own:
// callers MUST run this inside a domain.TxManager.WithinTx so a failed insert
// (unknown category id) rolls the delete back too, instead of silently
// leaving the user with zero preferences (same convention as
// restaurant.Related.Replace*).
func (r *Repository) Replace(ctx context.Context, userID uuid.UUID, categoryIDs []uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM user_cuisine_preferences WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("replace cuisine preferences: %w", err)
	}
	for _, categoryID := range categoryIDs {
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO user_cuisine_preferences (user_id, category_id, created_at)
			 VALUES ($1,$2,now())
			 ON CONFLICT (user_id, category_id) DO NOTHING`,
			userID, categoryID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("%w: unknown cuisine category", domain.ErrValidation)
			}
			return fmt.Errorf("replace cuisine preferences: %w", err)
		}
	}
	return nil
}
