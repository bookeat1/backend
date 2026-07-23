package bookings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// EndpointCreateBooking is the idempotency scope of POST /bookings. Keys are
// unique per (user, endpoint, key), so the same client-chosen key used on a
// different endpoint is a different key.
const EndpointCreateBooking = "POST /bookings"

// IdempotencyKey is the client-supplied retry token plus a hash of the request
// body. The transport layer computes RequestHash from the raw bytes it read —
// hashing a re-marshalled DTO would make field order and defaulting part of the
// identity, which is not what "same request" means.
type IdempotencyKey struct {
	Key         string
	RequestHash string
}

// IdempotentCreateUseCase is CreateUseCase made retry-safe (spec §7:
// "POST /api/bookings — Idempotency-Key header required").
//
// How it works:
//
//  1. the key is looked up first. A hit with a matching body hash replays the
//     stored response — no second booking is created. A hit with a DIFFERENT
//     body hash is a client bug: the same key must not mean two different
//     requests, so it is rejected with ErrAlreadyExists (HTTP 409);
//  2. on a miss, the booking and the key row are written inside ONE
//     transaction. Committing them separately would allow a crash between them
//     and a duplicate booking on the retry;
//  3. two concurrent first-attempts race on the unique index. Postgres makes
//     the loser wait on the duplicate key until the winner commits, so by the
//     time the loser sees ErrAlreadyExists the winning row is visible: the
//     loser rolls its own booking back and replays the winner's response.
//     This is exactly why the key insert is the last statement of the
//     transaction rather than the first — it holds the lock for the shortest
//     possible time.
type IdempotentCreateUseCase interface {
	CreateIdempotent(ctx context.Context, actor Actor, key IdempotencyKey, in CreateInput) (*BookingDetails, error)
}

type idempotentCreate struct {
	inner CreateUseCase
	keys  domain.IdempotencyRepository
	tx    domain.TxManager
}

// NewIdempotentCreateUseCase decorates a CreateUseCase with key-based replay.
func NewIdempotentCreateUseCase(inner CreateUseCase, keys domain.IdempotencyRepository, tx domain.TxManager) IdempotentCreateUseCase {
	return &idempotentCreate{inner: inner, keys: keys, tx: tx}
}

func (u *idempotentCreate) CreateIdempotent(ctx context.Context, actor Actor, key IdempotencyKey, in CreateInput) (*BookingDetails, error) {
	if key.Key == "" {
		return nil, fmt.Errorf("%w: Idempotency-Key header is required", domain.ErrValidation)
	}
	if actor.UserID == uuid.Nil {
		return nil, fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}

	if out, err := u.replay(ctx, actor.UserID, key); err != nil || out != nil {
		return out, err
	}

	var details *BookingDetails
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		created, err := u.inner.Create(ctx, actor, in)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(created)
		if err != nil {
			return fmt.Errorf("marshal idempotent response: %w", err)
		}
		if err := u.keys.Insert(ctx, &domain.IdempotencyRecord{
			ID:       uuid.New(),
			UserID:   actor.UserID,
			Endpoint: EndpointCreateBooking,
			Key:      key.Key, RequestHash: key.RequestHash,
			Response: payload,
		}); err != nil {
			return err
		}
		details = created
		return nil
	})
	if err == nil {
		return details, nil
	}
	// Lost the race (or the key was consumed between the lookup and the
	// insert): the booking we just made is rolled back, replay the winner's.
	if errors.Is(err, domain.ErrAlreadyExists) {
		if out, rerr := u.replay(ctx, actor.UserID, key); rerr != nil || out != nil {
			return out, rerr
		}
	}
	return nil, err
}

// replay returns the stored response for the key, or (nil, nil) when the key is
// unused. A body mismatch is reported as a conflict.
func (u *idempotentCreate) replay(ctx context.Context, userID uuid.UUID, key IdempotencyKey) (*BookingDetails, error) {
	rec, err := u.keys.Get(ctx, userID, EndpointCreateBooking, key.Key)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if rec.RequestHash != key.RequestHash {
		return nil, fmt.Errorf("%w: this Idempotency-Key was used with a different request body", domain.ErrAlreadyExists)
	}
	var out BookingDetails
	if err := json.Unmarshal(rec.Response, &out); err != nil {
		return nil, fmt.Errorf("decode stored idempotent response: %w", err)
	}
	return &out, nil
}
