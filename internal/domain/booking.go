package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BookingStatus is the lifecycle state of a booking, stored as VARCHAR.
type BookingStatus string

const (
	BookingPending   BookingStatus = "pending"
	BookingConfirmed BookingStatus = "confirmed"
	BookingWaitlist  BookingStatus = "waitlist"
	BookingArrived   BookingStatus = "arrived"
	BookingCompleted BookingStatus = "completed"
	BookingCancelled BookingStatus = "cancelled"
	BookingNoShow    BookingStatus = "no_show"
)

// Valid reports whether s is a known booking status.
func (s BookingStatus) Valid() bool {
	_, ok := bookingTransitions[s]
	return ok
}

// HoldsTable reports whether a booking in this status occupies its table(s).
// It mirrors the DB trigger that maintains booking_tables.active — keep both in
// sync (see migrations/0004_bookings.sql).
func (s BookingStatus) HoldsTable() bool {
	return s == BookingPending || s == BookingConfirmed || s == BookingArrived
}

// Terminal reports whether no further transition is allowed from s.
func (s BookingStatus) Terminal() bool { return len(bookingTransitions[s]) == 0 }

// bookingTransitions is the allowed status transition table. A status present
// with an empty set is valid but terminal.
//
//	pending ──confirm──▶ confirmed ──arrive──▶ arrived ──complete──▶ completed
//	   │                     │                    │
//	   └──cancel──▶ cancelled ◀──cancel───────────┘
//	pending ──waitlist──▶ waitlist ──confirm──▶ confirmed
//
// no_show is set by the background worker for bookings that were never marked
// arrived once ends_at + grace has passed. It is reachable ONLY from confirmed:
// a no-show is the guest breaking a promise the venue accepted, so a booking
// the venue never confirmed cannot be one. A pending / waitlist booking whose
// visit window has passed unanswered is closed as cancelled by the worker
// instead (cancelled_by = system).
var bookingTransitions = map[BookingStatus]map[BookingStatus]struct{}{
	BookingPending: {
		BookingConfirmed: {},
		BookingWaitlist:  {},
		BookingCancelled: {},
	},
	BookingWaitlist: {
		BookingConfirmed: {},
		BookingCancelled: {},
	},
	BookingConfirmed: {
		BookingArrived:   {},
		BookingCancelled: {},
		BookingNoShow:    {},
	},
	BookingArrived: {
		BookingCompleted: {},
		BookingCancelled: {},
	},
	BookingCompleted: {},
	BookingCancelled: {},
	BookingNoShow:    {},
}

// CanTransition reports whether from → to is an allowed status transition.
func CanTransition(from, to BookingStatus) bool {
	_, ok := bookingTransitions[from][to]
	return ok
}

// ValidateTransition returns nil when from → to is allowed, ErrValidation when
// either status is unknown, and ErrInvalidStatus otherwise (mapped to HTTP 422).
func ValidateTransition(from, to BookingStatus) error {
	if !from.Valid() || !to.Valid() {
		return ErrValidation
	}
	if !CanTransition(from, to) {
		return ErrInvalidStatus
	}
	return nil
}

// BookingSource records where a booking originated, stored as VARCHAR.
type BookingSource string

const (
	SourceApp    BookingSource = "app"
	SourceAdmin  BookingSource = "admin"
	SourcePhone  BookingSource = "phone"
	SourceWidget BookingSource = "widget"
)

// Valid reports whether s is a known booking source.
func (s BookingSource) Valid() bool {
	switch s {
	case SourceApp, SourceAdmin, SourcePhone, SourceWidget:
		return true
	}
	return false
}

// CancelledBy records who cancelled a booking, stored as VARCHAR.
type CancelledBy string

const (
	CancelledByGuest      CancelledBy = "guest"
	CancelledByRestaurant CancelledBy = "restaurant"
	CancelledBySystem     CancelledBy = "system"
)

// Valid reports whether c is a known cancellation actor.
func (c CancelledBy) Valid() bool {
	switch c {
	case CancelledByGuest, CancelledByRestaurant, CancelledBySystem:
		return true
	}
	return false
}

// BookingPolicy is the resolved booking policy for one restaurant: the global
// env defaults with the restaurant's non-NULL overrides applied. Resolution
// lives in usecase/bookings; this type only carries the result.
type BookingPolicy struct {
	Timezone            string // IANA name, e.g. "Asia/Almaty"
	Duration            time.Duration
	Buffer              time.Duration // added on both sides of the occupied slot
	Lead                time.Duration // minimum distance from now to starts_at
	HorizonDays         int           // furthest bookable day ahead
	CancelDeadline      time.Duration // guest may cancel until starts_at - CancelDeadline
	ConfirmSLA          time.Duration // pending → auto-confirm / escalation after this
	MaxGuestsPerBooking int
	AutoConfirm         bool
}

// BookingPolicyOverride is a restaurant's optional per-field override of the
// global policy. A nil field means "use the global default".
type BookingPolicyOverride struct {
	Timezone               *string
	BookingDurationMinutes *int
	BookingBufferMinutes   *int
	BookingLeadMinutes     *int
	BookingHorizonDays     *int
	CancelDeadlineMinutes  *int
	ConfirmSLAMinutes      *int
	MaxGuestsPerBooking    *int
	AutoConfirm            *bool
}

// Booking is a table reservation. ID equals the original Supabase id for
// migrated rows. PromotionID and EventID carry no FK on purpose: the promotion
// or event may be deleted, the booking must survive as a historical fact.
type Booking struct {
	ID                     uuid.UUID
	RestaurantID           uuid.UUID
	UserID                 *uuid.UUID // nil = guest booking
	Name                   string
	Phone                  string // as typed by the guest
	Email                  string // lower-cased
	PhoneNormalized        string // E.164
	Guests                 int
	StartsAt               time.Time
	EndsAt                 time.Time
	Status                 BookingStatus
	Source                 BookingSource
	Notes                  *string
	PromotionID            *uuid.UUID
	EventID                *uuid.UUID
	CreatedByAdmin         bool
	ForcedPlacement        bool // manager placed it despite an occupied table
	ConfirmedAt            *time.Time
	ArrivedAt              *time.Time
	CancelledAt            *time.Time
	CancelledBy            *CancelledBy
	CancellationReasonCode *string
	CancellationReason     *string
	LateNotificationSent   bool
	UserNotifiedLateAt     *time.Time
	UserLateMessage        *string
	Reminder60SentAt       *time.Time
	Reminder30SentAt       *time.Time
	OriginalBookingTime    *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// BookingFilter narrows a booking listing. Zero-value fields are ignored.
type BookingFilter struct {
	RestaurantID *uuid.UUID
	UserID       *uuid.UUID
	Statuses     []BookingStatus
	From         *time.Time // starts_at >= From
	To           *time.Time // starts_at <  To
	Page         int        // 1-based; <=0 means 1
	PerPage      int        // <=0 means default (20), capped at 100
}

// BookingRepository persists bookings. Get* return ErrNotFound when absent.
type BookingRepository interface {
	Create(ctx context.Context, b *Booking) error
	Update(ctx context.Context, b *Booking) error
	GetByID(ctx context.Context, id uuid.UUID) (*Booking, error)
	// List returns bookings matching f plus the total count, ordered by
	// starts_at DESC.
	List(ctx context.Context, f BookingFilter) ([]Booking, int, error)
	// UpdateStatus writes the new status and its timestamp columns. Call inside
	// a TxManager together with the history and outbox inserts.
	UpdateStatus(ctx context.Context, id uuid.UUID, status BookingStatus, at time.Time) error
	// ClaimDue locks up to limit bookings in the given statuses whose `by`
	// column is older than before, using FOR UPDATE SKIP LOCKED so parallel
	// workers do not collide. Results are ordered by that same column, oldest
	// first, so a batch smaller than the candidate set never starves the rows
	// that have been waiting longest.
	ClaimDue(ctx context.Context, statuses []BookingStatus, by ClaimColumn, before time.Time, limit int) ([]Booking, error)
}

// ClaimColumn names the timestamp ClaimDue compares against its cutoff. It is a
// closed set on purpose: the value reaches the SQL text, so it must never be
// caller-shaped data.
type ClaimColumn string

const (
	// ClaimByCreatedAt is the confirm-SLA clock: how long the venue has been
	// sitting on an unanswered request.
	ClaimByCreatedAt ClaimColumn = "created_at"
	// ClaimByEndsAt is the visit-window clock: used to close bookings whose
	// time has passed.
	ClaimByEndsAt ClaimColumn = "ends_at"
)

// Valid reports whether c is a known claim column.
func (c ClaimColumn) Valid() bool {
	return c == ClaimByCreatedAt || c == ClaimByEndsAt
}
