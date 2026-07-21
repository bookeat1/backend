package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Outbox implements domain.BookingOutboxRepository.
type Outbox struct{ pool sqltx.Querier }

// NewOutbox builds the transactional-outbox repository.
func NewOutbox(pool sqltx.Querier) *Outbox { return &Outbox{pool: pool} }

var _ domain.BookingOutboxRepository = (*Outbox)(nil)

const outboxCols = `id, booking_id, event_type, payload, created_at, published_at`

func (r *Outbox) Create(ctx context.Context, e *domain.BookingOutboxEvent) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	q := `INSERT INTO booking_outbox (` + outboxCols + `) VALUES ($1,$2,$3,$4,$5,$6)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, e.ID, e.BookingID,
		string(e.EventType), []byte(payload), e.CreatedAt, e.PublishedAt); err != nil {
		return mapWrite(err, "create booking outbox event")
	}
	return nil
}

// ClaimUnpublished locks undelivered events with FOR UPDATE SKIP LOCKED. It must
// run inside a TxManager transaction, otherwise the locks are dropped at once
// and two workers publish the same event twice.
func (r *Outbox) ClaimUnpublished(ctx context.Context, limit int) ([]domain.BookingOutboxEvent, error) {
	limit, _ = window(limit, 0)
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+outboxCols+` FROM booking_outbox
		 WHERE published_at IS NULL
		 ORDER BY created_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingOutboxEvent
	for rows.Next() {
		var e domain.BookingOutboxEvent
		var eventType string
		var payload []byte
		if err := rows.Scan(&e.ID, &e.BookingID, &eventType, &payload, &e.CreatedAt, &e.PublishedAt); err != nil {
			return nil, fmt.Errorf("claim outbox events: %w", err)
		}
		e.EventType = domain.BookingEventType(eventType)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Outbox) MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_outbox SET published_at=$2 WHERE id = ANY($1)`, ids, at); err != nil {
		return fmt.Errorf("mark outbox events published: %w", err)
	}
	return nil
}
