// Package idempotency is the Postgres implementation of the idempotency-key
// repository backing retry-safe mutating endpoints.
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const uniqueViolation = "23505"

// Repository implements domain.IdempotencyRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the idempotency-key repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.IdempotencyRepository = (*Repository)(nil)

const cols = `id, user_id, endpoint, idempotency_key, request_hash, response, created_at`

func (r *Repository) Get(ctx context.Context, userID uuid.UUID, endpoint, key string) (*domain.IdempotencyRecord, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM idempotency_keys
		 WHERE user_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
		userID, endpoint, key)
	var rec domain.IdempotencyRecord
	if err := row.Scan(&rec.ID, &rec.UserID, &rec.Endpoint, &rec.Key,
		&rec.RequestHash, &rec.Response, &rec.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: idempotency key", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get idempotency key: %w", err)
	}
	return &rec, nil
}

// Insert records the key. A duplicate key maps to ErrAlreadyExists so the
// caller can fall back to replaying the stored response — that is the branch
// two concurrent retries of the same request land in.
func (r *Repository) Insert(ctx context.Context, rec *domain.IdempotencyRecord) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	response := rec.Response
	if len(response) == 0 {
		response = []byte(`{}`)
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO idempotency_keys (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rec.ID, rec.UserID, rec.Endpoint, rec.Key, rec.RequestHash, response, rec.CreatedAt,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: idempotency key", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("insert idempotency key: %w", err)
	}
	return nil
}
