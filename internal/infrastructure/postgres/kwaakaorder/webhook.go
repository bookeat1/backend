package kwaakaorder

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Webhooks implements domain.KwaakaWebhookRepository (the inbox).
type Webhooks struct{ pool sqltx.Querier }

// NewWebhooks builds the inbox repository.
func NewWebhooks(pool sqltx.Querier) *Webhooks { return &Webhooks{pool: pool} }

var _ domain.KwaakaWebhookRepository = (*Webhooks)(nil)

func (r *Webhooks) Insert(ctx context.Context, e *domain.KwaakaWebhookEvent) (bool, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO kwaaka_webhook_events (id, kind, dedup_key, body) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (dedup_key) DO NOTHING`, e.ID, e.Kind, e.DedupKey, e.Body)
	if err != nil {
		return false, fmt.Errorf("insert kwaaka webhook: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Webhooks) LockDue(ctx context.Context, now time.Time, limit int) ([]domain.KwaakaWebhookEvent, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, kind, dedup_key, body, received_at, attempts, next_attempt_at
		   FROM kwaaka_webhook_events
		  WHERE processed_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		  ORDER BY received_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("lock kwaaka webhooks: %w", err)
	}
	defer rows.Close()
	var out []domain.KwaakaWebhookEvent
	for rows.Next() {
		var e domain.KwaakaWebhookEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.DedupKey, &e.Body, &e.ReceivedAt, &e.Attempts, &e.NextAttemptAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Webhooks) Finish(ctx context.Context, id uuid.UUID, outcome string, processErr *string, orderID *uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE kwaaka_webhook_events SET processed_at = now(), outcome = $2, process_error = $3,
		        kitchen_order_id = $4, next_attempt_at = NULL WHERE id = $1`, id, outcome, processErr, orderID)
	if err != nil {
		return fmt.Errorf("finish kwaaka webhook: %w", err)
	}
	return nil
}

func (r *Webhooks) Defer(ctx context.Context, id uuid.UUID, next time.Time) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE kwaaka_webhook_events SET attempts = attempts + 1, next_attempt_at = $2 WHERE id = $1`, id, next)
	if err != nil {
		return fmt.Errorf("defer kwaaka webhook: %w", err)
	}
	return nil
}

func (r *Webhooks) PruneProcessed(ctx context.Context, before time.Time, limit int) (int, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM kwaaka_webhook_events WHERE id IN
		   (SELECT id FROM kwaaka_webhook_events WHERE processed_at IS NOT NULL AND processed_at < $1 LIMIT $2)`,
		before, limit)
	if err != nil {
		return 0, fmt.Errorf("prune kwaaka webhooks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
