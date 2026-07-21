package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PaymentOutboxEventType names a payment event published outward (notifications
// to the guest and the venue, and anything downstream that cares about money).
type PaymentOutboxEventType string

const (
	EventPaymentCreated           PaymentOutboxEventType = "payment.created"
	EventPaymentAuthorized        PaymentOutboxEventType = "payment.authorized"
	EventPaymentCaptured          PaymentOutboxEventType = "payment.captured"
	EventPaymentVoided            PaymentOutboxEventType = "payment.voided"
	EventPaymentFailed            PaymentOutboxEventType = "payment.failed"
	EventPaymentExpired           PaymentOutboxEventType = "payment.expired"
	EventPaymentRefunded          PaymentOutboxEventType = "payment.refunded"
	EventPaymentPartiallyRefunded PaymentOutboxEventType = "payment.partially_refunded"
	EventPaymentSettled           PaymentOutboxEventType = "payment.settled"
)

// PaymentOutboxEvent is a transactional-outbox row, same contract as
// BookingOutboxEvent: inserted in the SAME transaction as the payment mutation
// it describes, drained by a worker. That is what makes "the money changed but
// nobody was told" impossible — either both happen or neither does.
//
// PublishedAt nil means not yet delivered.
type PaymentOutboxEvent struct {
	ID          uuid.UUID
	PaymentID   uuid.UUID
	EventType   PaymentOutboxEventType
	Payload     json.RawMessage
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// PaymentOutboxRepository persists and drains payment events.
type PaymentOutboxRepository interface {
	// Create inserts an event; call inside the same TxManager as the mutation.
	Create(ctx context.Context, e *PaymentOutboxEvent) error
	// ClaimUnpublished locks up to limit undelivered events using FOR UPDATE
	// SKIP LOCKED so parallel workers do not collide.
	ClaimUnpublished(ctx context.Context, limit int) ([]PaymentOutboxEvent, error)
	// ExistsForPayment reports whether an event of that type was already
	// recorded for the payment, so at-most-once events need no flag column.
	ExistsForPayment(ctx context.Context, paymentID uuid.UUID, eventType PaymentOutboxEventType) (bool, error)
	MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error
}
