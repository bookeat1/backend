package payment

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

// Events implements domain.PaymentEventRepository: raw acquirer callbacks,
// stored exactly as received before any interpretation (spec §7).
type Events struct{ pool sqltx.Querier }

// NewEvents builds the payment-event repository.
func NewEvents(pool sqltx.Querier) *Events { return &Events{pool: pool} }

var _ domain.PaymentEventRepository = (*Events)(nil)

const eventCols = `id, provider, provider_event_id, provider_payment_id, payment_id,
	event_type, payload, signature_valid, received_at, processed_at, process_error`

// Create stores a callback. idx_payment_events_provider_event is the whole
// idempotency mechanism for webhooks (spec §7): a redelivered callback hits
// this unique index and mapWrite turns it into domain.ErrAlreadyExists — the
// caller (webhookUseCase.HandleWebhook) re-reads the stored row instead of
// processing a duplicate.
func (r *Events) Create(ctx context.Context, e *domain.PaymentEvent) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now()
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	q := `INSERT INTO payment_events (` + eventCols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		e.ID, string(e.Provider), e.ProviderEventID, e.ProviderPaymentID, e.PaymentID,
		eventTypeToDB(e.EventType), []byte(payload), e.SignatureValid, e.ReceivedAt,
		e.ProcessedAt, e.ProcessError); err != nil {
		return mapWrite(err, "create payment event")
	}
	return nil
}

func (r *Events) GetByProviderEventID(ctx context.Context, provider domain.PaymentProvider, providerEventID string) (*domain.PaymentEvent, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+eventCols+` FROM payment_events WHERE provider=$1 AND provider_event_id=$2`,
		string(provider), providerEventID)
	e, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment event: %w", err)
	}
	return e, nil
}

// ClaimUnprocessed must run inside a TxManager transaction: FOR UPDATE SKIP
// LOCKED releases its locks as soon as the transaction ends, so two workers
// draining events outside one would both pick up the same row.
func (r *Events) ClaimUnprocessed(ctx context.Context, limit int) ([]domain.PaymentEvent, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+eventCols+` FROM payment_events
		 WHERE processed_at IS NULL
		 ORDER BY received_at, id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, window(limit))
	if err != nil {
		return nil, fmt.Errorf("claim unprocessed payment events: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("claim unprocessed payment events: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// MarkProcessed closes an event. Call ONLY after a successful apply, or after
// a deliberate, permanent "never applicable" verdict — see the domain doc
// comment on why this must never be called on a transient apply failure.
//
// An empty processErr (the success path) deliberately leaves any
// process_error already recorded by an earlier RecordProcessingError call
// untouched — the audit trail keeps "it failed twice, then succeeded on the
// third attempt" instead of erasing the history of a webhook that took a few
// tries. Same convention as fakeEventRepo.MarkProcessed.
func (r *Events) MarkProcessed(ctx context.Context, id uuid.UUID, at time.Time, processErr string) error {
	q := `UPDATE payment_events SET processed_at=$2 WHERE id=$1`
	args := []any{id, at}
	if processErr != "" {
		q = `UPDATE payment_events SET processed_at=$2, process_error=$3 WHERE id=$1`
		args = append(args, processErr)
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("mark payment event processed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RecordProcessingError stores why an apply attempt failed WITHOUT closing
// the event: processed_at is left untouched, so ClaimUnprocessed keeps
// offering this event to the next attempt.
func (r *Events) RecordProcessingError(ctx context.Context, id uuid.UUID, processErr string) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payment_events SET process_error=$2 WHERE id=$1`, id, processErr)
	if err != nil {
		return fmt.Errorf("record payment event processing error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetPaymentID backfills the payment this event turned out to belong to.
func (r *Events) SetPaymentID(ctx context.Context, id uuid.UUID, paymentID uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payment_events SET payment_id=$2 WHERE id=$1`, id, paymentID)
	if err != nil {
		return fmt.Errorf("set payment event payment id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func eventTypeToDB(t *domain.WebhookEventType) any {
	if t == nil {
		return nil
	}
	return string(*t)
}

func scanEvent(row scanner) (*domain.PaymentEvent, error) {
	var e domain.PaymentEvent
	var provider string
	var eventType *string
	if err := row.Scan(
		&e.ID, &provider, &e.ProviderEventID, &e.ProviderPaymentID, &e.PaymentID,
		&eventType, &e.Payload, &e.SignatureValid, &e.ReceivedAt, &e.ProcessedAt, &e.ProcessError,
	); err != nil {
		return nil, err
	}
	e.Provider = domain.PaymentProvider(provider)
	if eventType != nil {
		t := domain.WebhookEventType(*eventType)
		e.EventType = &t
	}
	return &e, nil
}
