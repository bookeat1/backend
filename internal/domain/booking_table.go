package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BookingTable links a booking to one physical table for a half-open time
// interval [SlotStart, SlotEnd). The slot ALREADY INCLUDES the restaurant's
// buffer on both sides — it is the interval during which the table is
// unavailable, not the guest-facing visit window.
//
// Active is written exclusively by the DB trigger on bookings.status
// (see migrations/0004_bookings.sql); never set it from application code. A
// GiST exclusion constraint over (table_id, slot) WHERE active makes
// overlapping active rows for the same table impossible — double booking is
// rejected by Postgres with a 23P01 exclusion_violation, which repositories map
// to ErrAlreadyExists.
type BookingTable struct {
	ID        uuid.UUID
	BookingID uuid.UUID
	TableID   uuid.UUID
	SlotStart time.Time
	SlotEnd   time.Time
	Active    bool
	CreatedAt time.Time
}

// TableBusyInterval is an occupied window for one table, used by the
// availability engine.
type TableBusyInterval struct {
	TableID uuid.UUID
	From    time.Time
	To      time.Time
}

// BookingTableRepository persists booking ↔ table links.
type BookingTableRepository interface {
	// Create inserts the links for one booking. Returns ErrAlreadyExists when
	// the exclusion constraint rejects an overlapping active slot.
	Create(ctx context.Context, links []BookingTable) error
	// ReplaceForBooking deletes the booking's links and inserts the given set
	// (call inside a TxManager). Same conflict semantics as Create.
	ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, links []BookingTable) error
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingTable, error)
	// ListBusy returns active occupied intervals for the restaurant's tables
	// overlapping [from, to).
	ListBusy(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]TableBusyInterval, error)
}
