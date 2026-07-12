// Package otp is the Postgres implementation of domain.OTPRepository.
package otp

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

var _ domain.OTPRepository = (*Repository)(nil)

func (r *Repository) Create(ctx context.Context, c *domain.OTPCode) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	q := `INSERT INTO otp_codes (id, phone, code_hash, channel, attempts, used_at, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		c.ID, c.Phone, c.CodeHash, c.Channel, c.Attempts, c.UsedAt, c.ExpiresAt, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("create otp: %w", err)
	}
	return nil
}

func (r *Repository) LatestActiveByPhone(ctx context.Context, phone string) (*domain.OTPCode, error) {
	q := `SELECT id, phone, code_hash, channel, attempts, used_at, expires_at, created_at
		FROM otp_codes
		WHERE phone = $1 AND used_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1`
	var c domain.OTPCode
	err := sqltx.From(ctx, r.pool).QueryRow(ctx, q, phone).Scan(
		&c.ID, &c.Phone, &c.CodeHash, &c.Channel, &c.Attempts, &c.UsedAt, &c.ExpiresAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("latest otp: %w", err)
	}
	return &c, nil
}

func (r *Repository) MarkUsed(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE otp_codes SET used_at = now() WHERE id = $1`, id)
	return err
}

func (r *Repository) IncrementAttempts(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE otp_codes SET attempts = attempts + 1 WHERE id = $1`, id)
	return err
}

func (r *Repository) CountSince(ctx context.Context, phone string, ts time.Time) (int, error) {
	var n int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM otp_codes WHERE phone = $1 AND created_at >= $2`, phone, ts).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count otp: %w", err)
	}
	return n, nil
}
