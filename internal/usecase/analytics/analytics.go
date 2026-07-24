// Package analytics ships BookEat's key product events to Amplitude. It does NOT
// sit in any request path: the guest/staff-facing usecases already write their
// state transitions into the transactional booking_outbox / payment_outbox
// (in the SAME transaction as the mutation). This package's background worker
// re-reads those outboxes through its own independent (created_at, id) cursor
// and forwards a NON-PII projection of each tracked row to Amplitude in
// batches. So analytics can never slow a booking down, and can never fail one:
// Amplitude being slow or down only delays the worker's own tick.
//
// No-PII guarantee: the mapper (mapper.go) builds each event's properties from
// an explicit allow-list of ids and coarse attributes (restaurant_id, guests,
// status, amount bucket, ...). The raw name / phone / email that the booking
// outbox payload happens to also carry are NEVER read here.
//
// No-op without a key: when AMPLITUDE_API_KEY is unset the dispatcher is built
// with a no-op Sender — every tick still advances the cursor as a cheap read,
// nothing is sent, nothing crashes (mirrors web-push without VAPID keys).
package analytics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EventType is Amplitude's event_type. Values are snake_case product events,
// stable strings a dashboard keys off — do not rename without a migration of
// the Amplitude charts.
type EventType string

const (
	EventBookingCreated   EventType = "booking_created"
	EventBookingConfirmed EventType = "booking_confirmed"
	EventBookingCancelled EventType = "booking_cancelled"
	EventNoShow           EventType = "no_show"
	EventPaymentCaptured  EventType = "payment_captured"
	EventPaymentRefunded  EventType = "payment_refunded"

	// Seams — defined so dashboards and the mapper agree on the names, but NOT
	// yet emitted. Their usecases (events/promos facades, ticketing) have no
	// transactional outbox for the worker to read; wiring them is a follow-up
	// (add a small outbox write, then a mapper branch here). See the PR.
	EventEventPublished  EventType = "event_published"
	EventPromoPublished  EventType = "promo_published"
	EventTicketPurchased EventType = "ticket_purchased"
)

// Event is the channel-agnostic, PII-free view of one product event handed to a
// Sender. It maps 1:1 onto an Amplitude event object.
type Event struct {
	Type EventType
	// UserID is the Amplitude user_id — the guest's account uuid when known,
	// empty for a walk-in / staff-created booking with no account. Never a
	// phone or an email.
	UserID string
	// DeviceID is a stable anonymous id (always set, >=5 chars) so Amplitude
	// can attribute and, crucially, DEDUPE: it dedupes on device_id+insert_id.
	DeviceID string
	// InsertID is the dedupe key = the source outbox row id. A reshipped row
	// carries the same InsertID, so Amplitude drops the duplicate.
	InsertID string
	// Time is when the business event happened (the outbox row's created_at).
	Time time.Time
	// Properties are non-PII event_properties only (ids + coarse attributes).
	Properties map[string]any
}

// Sender ships a batch of events. Implementations: the Amplitude HTTP client
// (internal/infrastructure/amplitude) and the in-package no-op.
//
// Contract: return a non-nil error ONLY for a transient failure the worker
// should retry (network error, HTTP 429/5xx). A permanently-rejected batch
// (HTTP 400/413) must be logged and dropped with a nil error, so one poison
// batch can never block the whole analytics stream forever.
type Sender interface {
	Send(ctx context.Context, batch []Event) error
}

// NewNoopSender returns a Sender that does nothing and never fails — used when
// AMPLITUDE_API_KEY is unset.
func NewNoopSender() Sender { return noopSender{} }

type noopSender struct{}

func (noopSender) Send(context.Context, []Event) error { return nil }

// SourceName identifies which transactional outbox a cursor tracks.
type SourceName string

const (
	SourceBookingOutbox SourceName = "booking_outbox"
	SourcePaymentOutbox SourceName = "payment_outbox"
)

// Sources is the fixed set of outboxes the worker drains, in a stable order.
func Sources() []SourceName { return []SourceName{SourceBookingOutbox, SourcePaymentOutbox} }

// SourceRow is a raw outbox row, source-agnostic. Only the columns the mapper
// needs are read; the mapper decodes Payload against an allow-list struct.
type SourceRow struct {
	ID        uuid.UUID
	EventType string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Cursor is the (created_at, id) high-water mark for one source. The zero value
// (epoch, nil uuid) sorts before every real row, so a source with no cursor row
// ships from the beginning.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// SourceReader reads outbox rows strictly after a cursor, ordered by
// (created_at, id). It reads the existing tables directly and never touches
// their published_at marker.
type SourceReader interface {
	ListSince(ctx context.Context, source SourceName, after Cursor, limit int) ([]SourceRow, error)
}

// CursorStore persists the per-source high-water mark.
type CursorStore interface {
	// Get returns the cursor for source, or the zero Cursor if none is stored.
	Get(ctx context.Context, source SourceName) (Cursor, error)
	Save(ctx context.Context, source SourceName, c Cursor) error
}
