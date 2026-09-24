package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// KitchenOrderStatus is the state of the order we send to the venue's POS
// (Kwaaka phase 2), stored as VARCHAR. Terminal states never leave: there is no
// re-send and no re-create (ADR-048).
type KitchenOrderStatus string

const (
	KitchenOrderSending       KitchenOrderStatus = "sending"
	KitchenOrderSent          KitchenOrderStatus = "sent"
	KitchenOrderCancelling    KitchenOrderStatus = "cancelling"
	KitchenOrderCancelled     KitchenOrderStatus = "cancelled"
	KitchenOrderFailed        KitchenOrderStatus = "failed"
	KitchenOrderFailedUnknown KitchenOrderStatus = "failed_unknown"
	KitchenOrderCancelFailed  KitchenOrderStatus = "cancel_failed"
)

// Valid reports whether s is a known status.
func (s KitchenOrderStatus) Valid() bool {
	switch s {
	case KitchenOrderSending, KitchenOrderSent, KitchenOrderCancelling, KitchenOrderCancelled,
		KitchenOrderFailed, KitchenOrderFailedUnknown, KitchenOrderCancelFailed:
		return true
	}
	return false
}

// Terminal reports whether no further transition is legal from s.
func (s KitchenOrderStatus) Terminal() bool {
	switch s {
	case KitchenOrderCancelled, KitchenOrderFailed, KitchenOrderFailedUnknown, KitchenOrderCancelFailed:
		return true
	}
	return false
}

// kitchenTransitions is the whole legal state machine (plan §2.3).
var kitchenTransitions = map[KitchenOrderStatus][]KitchenOrderStatus{
	KitchenOrderSending: {
		KitchenOrderSent, KitchenOrderFailed, KitchenOrderFailedUnknown, KitchenOrderCancelled,
		KitchenOrderCancelling, // cancel requested while sending, outcome "created"
	},
	KitchenOrderSent:       {KitchenOrderCancelling},
	KitchenOrderCancelling: {KitchenOrderCancelled, KitchenOrderCancelFailed},
}

// CanTransitionTo reports whether from → to is legal.
func (s KitchenOrderStatus) CanTransitionTo(to KitchenOrderStatus) bool {
	for _, t := range kitchenTransitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

// KitchenOrderTrigger says what made the booking go to the kitchen.
type KitchenOrderTrigger string

const (
	KitchenTriggerLead    KitchenOrderTrigger = "lead"    // paid pre-order, starts_at − lead
	KitchenTriggerArrived KitchenOrderTrigger = "arrived" // staff pressed "arrived"
)

// PosOrderState is the coarse POS-side state of an order. It only grows.
type PosOrderState string

const (
	PosStateOpen        PosOrderState = "open"
	PosStateBillPrinted PosOrderState = "bill_printed"
	PosStateClosed      PosOrderState = "closed"
	PosStateDeleted     PosOrderState = "deleted"
	PosStateUnknown     PosOrderState = "unknown"
)

// Rank orders POS states: open < bill_printed < closed = deleted. Unknown is 0
// (never applied). A first terminal state wins (equal rank = stale).
func (p PosOrderState) Rank() int {
	switch p {
	case PosStateOpen:
		return 1
	case PosStateBillPrinted:
		return 2
	case PosStateClosed, PosStateDeleted:
		return 3
	}
	return 0
}

// Terminal reports whether the POS order is over (closed or deleted).
func (p PosOrderState) Terminal() bool { return p == PosStateClosed || p == PosStateDeleted }

// Kitchen-order error codes stored in KitchenOrder.ErrorCode.
const (
	KitchenErrNoPosItems     = "no_pos_items"
	KitchenErrRejected       = "rejected"
	KitchenErrAuth           = "auth"
	KitchenErrWindowExpired  = "window_expired"
	KitchenErrAttemptsSpent  = "attempts_exhausted"
	KitchenErrCancelRefused  = "cancel_refused"
	KitchenErrCancelWindow   = "cancel_window_expired"
	KitchenErrPosCancelled   = "pos_cancelled"
	KitchenErrUnknownOutcome = "unknown_outcome"
)

// KitchenOrder is one row of kwaaka_kitchen_orders: the pre-order of one booking
// as sent to the POS. ID doubles as Kwaaka's order_id (the idempotency key).
type KitchenOrder struct {
	ID                 uuid.UUID
	BookingID          uuid.UUID
	RestaurantID       uuid.UUID
	KwaakaRestaurantID string
	KwaakaTableID      string
	TableShared        bool
	KwaakaOrderID      *string
	Status             KitchenOrderStatus
	Trigger            KitchenOrderTrigger
	Paid               bool
	PaidAmountMinor    *int64
	TotalMinor         int64
	Partial            bool
	RequestSnapshot    json.RawMessage
	BookingStartsAt    time.Time
	DeadlineAt         time.Time
	TableHoldUntil     time.Time
	TableReleasedAt    *time.Time
	Attempts           int
	OutcomeUnknown     bool
	NextAttemptAt      *time.Time
	LastError          *string
	ErrorCode          *string
	SentAt             *time.Time
	CancelRequestedAt  *time.Time
	CancelledAt        *time.Time
	CancelReason       *string
	PosStatusRaw       *string
	PosState           *PosOrderState
	PosStatusAt        *time.Time
	PosStatusSource    *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// KitchenOrderView is the read model shown to venue staff (booking card, list,
// "today"): never leaked to guests.
type KitchenOrderView struct {
	Status     KitchenOrderStatus
	SentAt     *time.Time
	PosState   *PosOrderState
	ErrorCode  *string
	Partial    bool
	TableLabel string
}

// KitchenCandidate is a booking that MAY need to go to the kitchen, selected
// without locks by ListCandidates; the claim re-checks everything under lock.
type KitchenCandidate struct {
	BookingID          uuid.UUID
	RestaurantID       uuid.UUID
	KwaakaRestaurantID string
}

// KitchenClaimItem is one non-cancelled pre-order line with the POS product
// mapping of its menu item.
type KitchenClaimItem struct {
	Name            string
	PriceMinor      int64
	Currency        string
	Quantity        int
	Comment         *string
	KwaakaProductID *string
}

// KitchenClaimSubject is everything the claim needs about a booking, read under
// FOR UPDATE of the booking row (lock order across the module: booking →
// settings, never the other way).
type KitchenClaimSubject struct {
	BookingID    uuid.UUID
	RestaurantID uuid.UUID
	Status       BookingStatus
	Name         string
	Phone        string
	Guests       int
	StartsAt     time.Time
	EndsAt       time.Time
	ConfirmedAt  *time.Time
	ArrivedAt    *time.Time
	Timezone     string // venue IANA zone
	Items        []KitchenClaimItem
	// Paid: a preorder payment in status exactly "captured".
	Paid       bool
	PaidMinor  int64
	CapturedAt *time.Time
	KwaakaID   string // restaurants.kwaaka_restaurant_id at claim time
}

// KitchenTableLoad counts our own live orders per POS table.
type KitchenTableLoad struct {
	Inflight map[string]int
	Live     map[string]int
}

// KitchenOrderRepository persists kwaaka_kitchen_orders.
type KitchenOrderRepository interface {
	// ListCandidates returns unlocked candidates: venue enabled, linkage equals
	// the pool's, pool non-empty, booking confirmed/arrived within ±12h of now,
	// no order row yet, at least one non-cancelled item.
	ListCandidates(ctx context.Context, now time.Time, limit int) ([]KitchenCandidate, error)
	// LockClaimSubject reads the booking FOR UPDATE with items and payment.
	// Must run inside a transaction. ErrNotFound when the booking is gone.
	LockClaimSubject(ctx context.Context, bookingID uuid.UUID) (*KitchenClaimSubject, error)
	// Insert stores a new row; created=false (no error) when the booking
	// already has one (ON CONFLICT (booking_id) DO NOTHING).
	Insert(ctx context.Context, o *KitchenOrder) (created bool, err error)
	GetByID(ctx context.Context, id uuid.UUID) (*KitchenOrder, error)
	GetByBookingID(ctx context.Context, bookingID uuid.UUID) (*KitchenOrder, error)
	// GetByPosOrderID matches kwaaka_order_id.
	GetByPosOrderID(ctx context.Context, posOrderID string) (*KitchenOrder, error)
	// ListByBookingIDs is the batch read for list screens (no N+1).
	ListByBookingIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]KitchenOrderView, error)
	// LeaseDue takes up to limit rows in sending/cancelling with
	// next_attempt_at <= now (SKIP LOCKED), bumps attempts and pushes
	// next_attempt_at to now+lease, and returns them as they are after the bump.
	LeaseDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]KitchenOrder, error)
	// CompareAndSet writes every mutable field of o iff the stored row still has
	// status wantStatus and attempts wantAttempts. swapped=false = someone else
	// (webhook, cancel sweep) moved the row; the caller re-reads and reconciles.
	CompareAndSet(ctx context.Context, o *KitchenOrder, wantStatus KitchenOrderStatus, wantAttempts int) (swapped bool, err error)
	// TableLoads counts inflight and live orders per table for a venue.
	TableLoads(ctx context.Context, restaurantID uuid.UUID, now time.Time) (KitchenTableLoad, error)
	// ListCancelledPending returns sending/sent rows whose booking is cancelled
	// and that have no cancel_requested_at yet.
	ListCancelledPending(ctx context.Context, limit int) ([]KitchenOrder, error)
	// ListRescheduled returns sent rows whose booking starts_at differs from
	// booking_starts_at, with the booking's current starts_at.
	ListRescheduled(ctx context.Context, limit int) ([]KitchenReschedule, error)
	// ListPollDue returns sent/cancelling rows with a non-terminal pos_state
	// whose pos_status_at is empty or older than staleBefore, sent within 12h.
	ListPollDue(ctx context.Context, now, staleBefore time.Time, limit int) ([]KitchenOrder, error)
}

// KitchenReschedule pairs a sent order with its booking's new start.
type KitchenReschedule struct {
	Order       KitchenOrder
	NewStartsAt time.Time
}
