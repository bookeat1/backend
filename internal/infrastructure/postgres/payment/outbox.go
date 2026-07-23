package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Outbox implements domain.PaymentOutboxRepository — same shape and worker
// contract as booking.Outbox: written in the same transaction as the payment
// mutation it describes, drained by a separate worker pass.
type Outbox struct{ pool sqltx.Querier }

// NewOutbox builds the transactional-outbox repository for payments.
func NewOutbox(pool sqltx.Querier) *Outbox { return &Outbox{pool: pool} }

var _ domain.PaymentOutboxRepository = (*Outbox)(nil)

const paymentOutboxCols = `id, payment_id, event_type, payload, created_at, published_at`

func (r *Outbox) Create(ctx context.Context, e *domain.PaymentOutboxEvent) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	q := `INSERT INTO payment_outbox (` + paymentOutboxCols + `) VALUES ($1,$2,$3,$4,$5,$6)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, e.ID, e.PaymentID,
		string(e.EventType), []byte(payload), e.CreatedAt, e.PublishedAt); err != nil {
		return mapWrite(err, "create payment outbox event")
	}
	return nil
}

// ClaimUnpublished must run inside a TxManager transaction, same locking
// caveat as booking.Outbox.ClaimUnpublished.
func (r *Outbox) ClaimUnpublished(ctx context.Context, limit int) ([]domain.PaymentOutboxEvent, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+paymentOutboxCols+` FROM payment_outbox
		 WHERE published_at IS NULL
		 ORDER BY created_at, id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim payment outbox events: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentOutboxEvent
	for rows.Next() {
		e, err := scanOutboxEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("claim payment outbox events: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ExistsForPayment backs the at-most-once outbox events (e.g. payment.settled
// is only ever written once per payment, see refundUseCase.settleWithNoRefund).
func (r *Outbox) ExistsForPayment(ctx context.Context, paymentID uuid.UUID, eventType domain.PaymentOutboxEventType) (bool, error) {
	var exists bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_outbox WHERE payment_id=$1 AND event_type=$2)`,
		paymentID, string(eventType)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check payment outbox event: %w", err)
	}
	return exists, nil
}

func (r *Outbox) MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payment_outbox SET published_at=$2 WHERE id = ANY($1)`, ids, at); err != nil {
		return fmt.Errorf("mark payment outbox events published: %w", err)
	}
	return nil
}

func scanOutboxEvent(row scanner) (*domain.PaymentOutboxEvent, error) {
	var e domain.PaymentOutboxEvent
	var eventType string
	var payload []byte
	if err := row.Scan(&e.ID, &e.PaymentID, &eventType, &payload, &e.CreatedAt, &e.PublishedAt); err != nil {
		return nil, err
	}
	e.EventType = domain.PaymentOutboxEventType(eventType)
	e.Payload = payload
	return &e, nil
}
