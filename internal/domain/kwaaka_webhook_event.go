package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Kwaaka webhook kinds (inbox rows).
const (
	KwaakaWebhookOrderStatus   = "order_status"
	KwaakaWebhookReserveStatus = "reserve_status"
)

// Inbox processing outcomes.
const (
	WebhookOutcomeApplied             = "applied"
	WebhookOutcomeStale               = "stale"
	WebhookOutcomeUnknownStatus       = "unknown_status"
	WebhookOutcomeIgnoredUnknownOrder = "ignored_unknown_order"
	WebhookOutcomeUnparseable         = "unparseable"
	WebhookOutcomeStored              = "stored" // reserve-status skeleton: kept, nothing applied
)

// KwaakaWebhookEvent is one received webhook, stored as it came.
type KwaakaWebhookEvent struct {
	ID             uuid.UUID
	Kind           string
	DedupKey       string
	Body           []byte
	ReceivedAt     time.Time
	Attempts       int
	NextAttemptAt  *time.Time
	ProcessedAt    *time.Time
	Outcome        *string
	ProcessError   *string
	KitchenOrderID *uuid.UUID
}

// KwaakaOrderStatusEvent is a parsed order-status webhook or poll result, in
// domain terms. Only infrastructure/kwaaka knows the wire format.
type KwaakaOrderStatusEvent struct {
	OrderID         string // Kwaaka order id or our order_id
	Raw             string // status string as received
	State           PosOrderState
	TableIDs        []string
	WhenBillPrinted *time.Time
	WhenClosed      *time.Time
}

// KwaakaWebhookRepository is the inbox.
type KwaakaWebhookRepository interface {
	// Insert stores the event; inserted=false when dedup_key already exists.
	Insert(ctx context.Context, e *KwaakaWebhookEvent) (inserted bool, err error)
	// LockDue returns unprocessed events with next_attempt_at null or due,
	// FOR UPDATE SKIP LOCKED, oldest first. Must run inside a transaction.
	LockDue(ctx context.Context, now time.Time, limit int) ([]KwaakaWebhookEvent, error)
	// Finish marks the event processed with outcome.
	Finish(ctx context.Context, id uuid.UUID, outcome string, processErr *string, kitchenOrderID *uuid.UUID) error
	// Defer bumps attempts and schedules the next try.
	Defer(ctx context.Context, id uuid.UUID, next time.Time) error
	// PruneProcessed deletes up to limit processed events older than before.
	PruneProcessed(ctx context.Context, before time.Time, limit int) (int, error)
}
