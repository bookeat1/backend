package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PaymentEvent is a raw acquirer callback stored exactly as received, BEFORE
// any interpretation — including callbacks whose signature did not verify
// (SignatureValid = false). Those are evidence, not input: nothing may act on
// them, and nothing may throw them away either.
//
// PaymentID is optional and carries no foreign key: a callback may reference a
// provider payment id we do not know. Such an event is stored and escalated for
// investigation; it never conjures a payment out of thin air (spec §7).
type PaymentEvent struct {
	ID                uuid.UUID
	Provider          PaymentProvider
	ProviderEventID   string
	ProviderPaymentID *string
	PaymentID         *uuid.UUID
	EventType         *WebhookEventType
	Payload           json.RawMessage
	SignatureValid    bool
	ReceivedAt        time.Time
	ProcessedAt       *time.Time
	ProcessError      *string
}

// PaymentEventRepository stores and drains acquirer callbacks.
type PaymentEventRepository interface {
	// Create stores a callback. It returns ErrAlreadyExists when
	// (provider, provider_event_id) is already known — that is the whole
	// idempotency mechanism for webhooks: an acquirer redelivers, we process
	// once (spec §7).
	Create(ctx context.Context, e *PaymentEvent) error
	GetByProviderEventID(ctx context.Context, provider PaymentProvider, providerEventID string) (*PaymentEvent, error)
	// ClaimUnprocessed locks up to limit unprocessed events using FOR UPDATE
	// SKIP LOCKED, oldest first. The HTTP handler answers the acquirer in
	// milliseconds; the business logic runs from here (spec §7).
	ClaimUnprocessed(ctx context.Context, limit int) ([]PaymentEvent, error)
	// MarkProcessed closes an event. A non-empty processErr records why it
	// could not be applied without pretending it was.
	MarkProcessed(ctx context.Context, id uuid.UUID, at time.Time, processErr string) error
}
