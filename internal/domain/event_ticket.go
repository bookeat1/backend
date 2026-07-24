package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TicketStatus is an event ticket's lifecycle state, stored as VARCHAR
// (validated here, never a Postgres ENUM). A ticket is born `pending` when the
// seat is reserved, becomes `paid` when its payment is captured, and frees its
// held seat by moving to `cancelled` (payment failed/expired/voided, or the
// reservation was released) or `refunded` (a paid ticket was refunded).
//
// Capacity is HELD by `pending` and `paid` tickets only — see HoldsCapacity.
type TicketStatus string

const (
	// TicketPending is a reserved-but-unpaid ticket; it holds capacity while its
	// payment is in flight.
	TicketPending TicketStatus = "pending"
	// TicketPaid is a ticket whose payment was captured; it holds capacity.
	TicketPaid TicketStatus = "paid"
	// TicketCancelled is a ticket whose payment failed/expired/voided or whose
	// reservation was released before payment; it frees its capacity.
	TicketCancelled TicketStatus = "cancelled"
	// TicketRefunded is a paid ticket that was later refunded; it frees its
	// capacity.
	TicketRefunded TicketStatus = "refunded"
)

// Valid reports whether s is a known ticket status.
func (s TicketStatus) Valid() bool {
	switch s {
	case TicketPending, TicketPaid, TicketCancelled, TicketRefunded:
		return true
	}
	return false
}

// HoldsCapacity reports whether a ticket in this status counts against the
// event's capacity. It is the single source of truth for the capacity SUM — the
// postgres repository's SoldCount filter mirrors it exactly, keep both in sync.
func (s TicketStatus) HoldsCapacity() bool {
	return s == TicketPending || s == TicketPaid
}

// ticketTransitions is the allowed ticket status transition table. A status
// present with an empty set is terminal.
//
//	pending ──pay──▶ paid ──refund──▶ refunded
//	   │              │
//	   └──release──▶ cancelled (also: paid never goes to cancelled)
//
// A ticket's status is a projection of its payment's status (see
// usecase/tickets sync): pending→paid on capture, pending→cancelled on
// fail/expire/void, paid→refunded on a refund. There is no way back into
// pending and no way out of cancelled/refunded.
var ticketTransitions = map[TicketStatus]map[TicketStatus]struct{}{
	TicketPending: {
		TicketPaid:      {},
		TicketCancelled: {},
	},
	TicketPaid: {
		TicketRefunded: {},
	},
	TicketCancelled: {},
	TicketRefunded:  {},
}

// CanTicketTransition reports whether from → to is an allowed ticket status
// transition.
func CanTicketTransition(from, to TicketStatus) bool {
	_, ok := ticketTransitions[from][to]
	return ok
}

// EventTicket is one guest's purchase of N tickets for a ticketed event. It is
// a financial record: UnitPriceMinor/TotalMinor are frozen at purchase time
// (the event's ticket price may change afterwards, e.g. a price step every 200
// sold — the ticket keeps the price it was bought at). RestaurantID is
// denormalised out of the event so admin listings and RBAC scoping never need a
// join.
//
// UserID is nil for a guest checkout without an account; GuestName/GuestPhone/
// GuestEmail carry the contact details in that case (and a convenience copy for
// an account buyer). Money is integer minor units, never a float.
type EventTicket struct {
	ID             uuid.UUID
	EventID        uuid.UUID
	RestaurantID   uuid.UUID
	UserID         *uuid.UUID
	GuestName      string
	GuestPhone     string // normalised, E.164-ish
	GuestEmail     string
	Quantity       int
	UnitPriceMinor int64
	TotalMinor     int64
	Currency       Currency
	Status         TicketStatus
	// PaymentID links to the payments row that pays for this ticket. nil until
	// the payment intent is created. Not a DB foreign key (payments references
	// event_tickets, a mutual FK would be a cycle) — same FK-less back-reference
	// as PaymentEvent.PaymentID.
	PaymentID              *uuid.UUID
	PurchaseIdempotencyKey string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// EventTicketCounts is the admin "tickets sold" aggregate for one event: how
// many tickets and how much revenue, grouped by whether they are still holding
// (pending), paid, or released (cancelled/refunded). RevenuePaidMinor counts
// only captured (paid) money — a pending or refunded ticket is not revenue.
type EventTicketCounts struct {
	EventID          uuid.UUID
	PendingTickets   int   // number of pending ticket rows
	PendingQuantity  int   // sum of quantity over pending rows
	PaidTickets      int   // number of paid ticket rows
	PaidQuantity     int   // sum of quantity over paid rows (seats sold)
	RefundedTickets  int   // number of refunded ticket rows
	CancelledTickets int   // number of cancelled ticket rows
	RevenuePaidMinor int64 // sum of total_minor over paid rows only
	Currency         Currency
}

// EventTicketRepository persists event tickets. Get* return ErrNotFound when
// absent.
type EventTicketRepository interface {
	// LockEventForCapacity takes a row lock on the event (SELECT ... FOR UPDATE)
	// so concurrent buyers of the SAME event serialise through the capacity
	// check below instead of racing it. Returns ErrNotFound if the event is
	// absent. MUST be called inside a TxManager transaction, before SoldCount +
	// Create, or the no-oversell guarantee does not hold.
	LockEventForCapacity(ctx context.Context, eventID uuid.UUID) error
	// SoldCount returns the sum of quantity over tickets that currently HOLD
	// capacity (pending + paid) for the event — mirrors
	// TicketStatus.HoldsCapacity. Read inside the same transaction as
	// LockEventForCapacity to get a race-safe remaining-capacity figure.
	SoldCount(ctx context.Context, eventID uuid.UUID) (int, error)
	// Create inserts a new ticket. A duplicate (event_id, purchase_idempotency_key)
	// maps to ErrAlreadyExists (the idempotent-create guard).
	Create(ctx context.Context, t *EventTicket) error
	// GetByID returns a ticket by id regardless of status.
	GetByID(ctx context.Context, id uuid.UUID) (*EventTicket, error)
	// GetByIdempotencyKey resolves a buyer's retry token for an event, so a
	// retried purchase replays its own ticket instead of reserving new seats.
	// Returns ErrNotFound when unused.
	GetByIdempotencyKey(ctx context.Context, eventID uuid.UUID, key string) (*EventTicket, error)
	// SetPaymentID backfills the payment link once the payment intent exists.
	SetPaymentID(ctx context.Context, id, paymentID uuid.UUID) error
	// CompareAndSwapStatus is the DB-level guard for every ticket status change:
	// a single `UPDATE event_tickets SET status=$to, updated_at=$at WHERE id=$id
	// AND status=$from`. Returns ErrAlreadyExists when zero rows matched (the
	// ticket already moved away from `from`) so a concurrent/duplicate sync moves
	// a ticket at most once. Callers must have validated CanTicketTransition.
	CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to TicketStatus, at time.Time) error
	// ListByEvent returns an event's tickets (optionally status-filtered),
	// newest first with id as a stable tie-breaker, paginated, plus the total.
	ListByEvent(ctx context.Context, eventID uuid.UUID, statuses []TicketStatus, page, perPage int) ([]EventTicket, int, error)
	// ListByUser returns a guest's own tickets, newest first, paginated.
	ListByUser(ctx context.Context, userID uuid.UUID, page, perPage int) ([]EventTicket, int, error)
	// Counts returns the admin "tickets sold" aggregate for an event.
	Counts(ctx context.Context, eventID uuid.UUID) (EventTicketCounts, error)
}
