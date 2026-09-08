// Package refreshtoken is the Postgres implementation of
// domain.RefreshTokenRepository.
package refreshtoken

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

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.RefreshTokenRepository = (*Repository)(nil)

func (r *Repository) Create(ctx context.Context, t *domain.RefreshToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	q := `INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked_at, user_agent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		t.ID, t.UserID, t.TokenHash, t.ExpiresAt, t.RevokedAt, t.UserAgent, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *Repository) GetByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error) {
	q := `SELECT id, user_id, token_hash, expires_at, revoked_at, user_agent, created_at
		FROM refresh_tokens WHERE token_hash = $1`
	var t domain.RefreshToken
	err := sqltx.From(ctx, r.pool).QueryRow(ctx, q, tokenHash).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.RevokedAt, &t.UserAgent, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", err)
	}
	return &t, nil
}

func (r *Repository) Revoke(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, id)
	return err
}

func (r *Repository) RevokeAllByUser(ctx context.Context, userID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("revoke all refresh tokens: %w", err)
	}
	return nil
}
