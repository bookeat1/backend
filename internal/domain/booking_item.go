package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BookingItemStatus is the state of a pre-ordered line, stored as VARCHAR.
type BookingItemStatus string

const (
	BookingItemPending   BookingItemStatus = "pending"
	BookingItemConfirmed BookingItemStatus = "confirmed"
	BookingItemCancelled BookingItemStatus = "cancelled"
	BookingItemServed    BookingItemStatus = "served"
)

// Valid reports whether s is a known booking item status.
func (s BookingItemStatus) Valid() bool {
	switch s {
	case BookingItemPending, BookingItemConfirmed, BookingItemCancelled, BookingItemServed:
		return true
	}
	return false
}

// BookingItem is a menu position pre-ordered with a booking. ItemName and
// PriceMinor are denormalized on purpose: MenuItemID may become nil when the
// dish is removed, and the price is frozen at booking time (payments must use
// this value, not the current menu price). PriceMinor is in minor units
// (tiyn); Currency is ISO-4217 (KZT for now).
type BookingItem struct {
	ID         uuid.UUID
	BookingID  uuid.UUID
	MenuItemID *uuid.UUID
	ItemName   string
	PriceMinor int64
	Currency   string
	Quantity   int
	Status     BookingItemStatus
	Comment    *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TotalMinor returns the line total in minor units.
func (i BookingItem) TotalMinor() int64 { return i.PriceMinor * int64(i.Quantity) }

// BookingItemRepository persists pre-ordered menu positions.
type BookingItemRepository interface {
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingItem, error)
	// ReplaceForBooking deletes the booking's items and inserts the given set
	// (call inside a TxManager).
	ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, items []BookingItem) error
	Create(ctx context.Context, items []BookingItem) error
	SetStatus(ctx context.Context, id uuid.UUID, status BookingItemStatus) error
}
