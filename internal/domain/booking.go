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

// StatusesHoldingTable lists the statuses HoldsTable reports true for, for
// callers that need them as a filter value rather than as a predicate (e.g.
// "every booking of this venue that still occupies a seat").
func StatusesHoldingTable() []BookingStatus {
	return []BookingStatus{BookingPending, BookingConfirmed, BookingArrived}
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
	// CapacityMode selects the availability engine for this venue; see
	// CapacityMode. Always a valid value after resolution (never empty).
	CapacityMode CapacityMode
	// CapacitySeats is the guests the venue can seat at once. Meaningful only
	// when CapacityMode is CapacityModeSeats; zero otherwise.
	CapacitySeats int
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
	// BookingCapacityMode / BookingCapacitySeats are the table-less switch
	// (migration 0054). They follow the same PATCH semantics as the fields
	// above — nil means "leave this column alone" — but a NULL
	// booking_capacity_mode is not an env-backed default: it simply means
	// CapacityModeTables, the behaviour every venue had before 0054.
	BookingCapacityMode  *CapacityMode
	BookingCapacitySeats *int
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
	// ListLiveForReconcile returns, in ONE statement, every booking of the venue
	// in the given statuses whose starts_at >= from, ordered by starts_at then
	// id, capped at limit (and at MaxReconcileBookings whatever the caller asks
	// for).
	//
	// It exists because List's OFFSET pagination cannot be used to read a set
	// that must be reconciled as a whole: the enclosing transaction is READ
	// COMMITTED, so every page is a fresh snapshot, and a status change that
	// commits between two pages (a guest cancelling — status.go takes no venue
	// lock) shifts the window and one row is never returned at all. A single
	// statement sees a single snapshot, which is the property the caller needs.
	//
	// The order is ascending and total (starts_at, then id) because the caller
	// makes decisions whose outcome depends on the order it walks the set in,
	// and a retry of the same operation must reach the same answer.
	ListLiveForReconcile(ctx context.Context, restaurantID uuid.UUID, from time.Time, statuses []BookingStatus, limit int) ([]Booking, error)
}

// MaxReconcileBookings is the hard cap on how many bookings one whole-set
// reconciliation (a capacity-mode switch) may load and rewrite.
//
// It is a real product limit, not a paging size: the switch runs 4-5 statements
// per booking under the venue's advisory lock, inside an HTTP request whose
// write timeout is 15s (bootstrap/app.go). Past some size the switch cannot
// finish in time — and the failure mode without a cap is the worst one
// available: the request dies on timeout, the transaction rolls back, and the
// venue lock is held meanwhile so ordinary booking creates block behind a
// switch that will never succeed. With the cap the switch is refused
// immediately, loudly, and with something staff can act on.
const MaxReconcileBookings = 300

// BookingReminderRepository backs the pre-visit guest reminder pass. It is a
// port of its own, separate from BookingRepository, because the reminder marker
// (bookings.guest_reminder_sent_at) is deliberately NOT a field of Booking: it
// is worker bookkeeping written through these two methods only, so a full-row
// Update from any other path can never clobber it.
type BookingReminderRepository interface {
	// ClaimDueReminders locks up to limit bookings whose visit starts inside
	// (from, to] and that have not been reminded yet, with FOR UPDATE SKIP
	// LOCKED. It returns only bookings that are still LIVE (pending / confirmed
	// / waitlist) and belong to a guest ACCOUNT (user_id NOT NULL) — a cancelled
	// visit is never reminded, and a phone booking has no device to remind.
	//
	// A booking created after its own reminder point (the guest booked less than
	// the reminder lead before the visit) is skipped: they just made it, a
	// "don't forget" a minute later is noise.
	ClaimDueReminders(ctx context.Context, from, to time.Time, limit int) ([]Booking, error)
	// MarkReminderSent stamps the reminder marker and reports whether THIS call
	// is the one that stamped it. It is the idempotency arbiter: false means the
	// booking was already reminded or is no longer live, and the caller must not
	// emit the event. Call it inside the same transaction as the outbox insert,
	// so the stamp and the event commit together or not at all.
	MarkReminderSent(ctx context.Context, bookingID uuid.UUID, at time.Time) (bool, error)
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
