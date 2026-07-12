// Package usercredential is the Postgres implementation of
// domain.UserCredentialRepository.
package usercredential

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

var _ domain.UserCredentialRepository = (*Repository)(nil)

func (r *Repository) Upsert(ctx context.Context, c *domain.UserCredential) error {
	q := `INSERT INTO user_credentials (user_id, password_hash)
		VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash`
	if _, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q, c.UserID, c.PasswordHash); err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}
	return nil
}

func (r *Repository) GetByUserID(ctx context.Context, userID uuid.UUID) (*domain.UserCredential, error) {
	q := `SELECT user_id, password_hash FROM user_credentials WHERE user_id = $1`
	var c domain.UserCredential
	err := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, userID).Scan(&c.UserID, &c.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	return &c, nil
}
