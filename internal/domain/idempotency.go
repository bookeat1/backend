package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// IdempotencyRecord is the stored outcome of one mutating request, keyed by the
// client's Idempotency-Key header. Scope is (UserID, Endpoint, Key): the key is
// a client-chosen string, so it is only unique per caller and per endpoint.
//
// RequestHash is a hash of the raw request body. A retry with the same key but
// a different body is a client error, not a replay — see the migration
// 0005_idempotency.sql comment for the full rationale.
type IdempotencyRecord struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Endpoint    string
	Key         string
	RequestHash string
	Response    []byte // stored success payload, replayed verbatim
	CreatedAt   time.Time
}

// IdempotencyRepository persists request keys and their recorded responses.
type IdempotencyRepository interface {
	// Get returns ErrNotFound when the key has not been used yet.
	Get(ctx context.Context, userID uuid.UUID, endpoint, key string) (*IdempotencyRecord, error)
	// Insert returns ErrAlreadyExists when the key is already taken. Call it
	// inside the same TxManager transaction as the mutation it records.
	Insert(ctx context.Context, r *IdempotencyRecord) error
}
