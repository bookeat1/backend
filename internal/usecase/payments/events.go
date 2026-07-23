package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// paymentEventPayload is the notification-layer view of a payment: only what
// an edge delivery layer needs to render a message, never the full row (no
// idempotency key, no raw acquirer payload).
type paymentEventPayload struct {
	ID           uuid.UUID             `json:"id"`
	BookingID    uuid.UUID             `json:"booking_id"`
	RestaurantID uuid.UUID             `json:"restaurant_id"`
	Purpose      domain.PaymentPurpose `json:"purpose"`
	Status       domain.PaymentStatus  `json:"status"`
	AmountMinor  int64                 `json:"amount_minor"`
	Currency     domain.Currency       `json:"currency"`
}

func newPaymentEventPayload(p *domain.Payment) paymentEventPayload {
	return paymentEventPayload{
		ID: p.ID, BookingID: p.BookingID, RestaurantID: p.RestaurantID,
		Purpose: p.Purpose, Status: p.Status, AmountMinor: p.AmountMinor, Currency: p.Currency,
	}
}

// publishPaymentEvent inserts one outbox event describing the payment's
// current state. Must run inside the same TxManager transaction as the
// mutation it describes (spec: transactional outbox, same contract as
// booking_outbox).
func publishPaymentEvent(ctx context.Context, outbox domain.PaymentOutboxRepository, p *domain.Payment, eventType domain.PaymentOutboxEventType, at time.Time) error {
	payload, err := json.Marshal(newPaymentEventPayload(p))
	if err != nil {
		return fmt.Errorf("marshal payment outbox payload: %w", err)
	}
	return outbox.Create(ctx, &domain.PaymentOutboxEvent{
		ID: uuid.New(), PaymentID: p.ID, EventType: eventType, Payload: payload, CreatedAt: at,
	})
}
