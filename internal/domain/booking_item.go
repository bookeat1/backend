package domain

import (
	"context"
	"fmt"
	"math"
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

// TotalMinorChecked returns the line total (price × quantity) in minor units,
// guarding against int64 overflow — the same defensive posture the rest of the
// money domain takes (see PriceStringToMinor). A line price is restaurant-
// controlled and quantities are bounded, so this never triggers on real data;
// it exists so a corrupt/absurd stored value can never silently WRAP a charge
// into a negative or otherwise wrong amount. Money paths (SumPreorderItems)
// use this; TotalMinor is a display convenience over it.
func (i BookingItem) TotalMinorChecked() (int64, error) {
	q := int64(i.Quantity)
	// Only two positive operands can overflow into a wrong positive here; a
	// non-positive operand cannot exceed the input magnitudes.
	if q > 0 && i.PriceMinor > 0 && i.PriceMinor > math.MaxInt64/q {
		return 0, fmt.Errorf("%w: line total overflows (price %d × qty %d)", ErrValidation, i.PriceMinor, i.Quantity)
	}
	return i.PriceMinor * q, nil
}

// TotalMinor returns the line total in minor units for DISPLAY. On the
// (practically unreachable) overflow it returns math.MaxInt64 rather than a
// wrapped negative, so a corrupt value can never render as a plausible small
// or negative amount. Anything that CHARGES money must use TotalMinorChecked /
// SumPreorderItems, which surface the overflow as an explicit error instead.
func (i BookingItem) TotalMinor() int64 {
	t, err := i.TotalMinorChecked()
	if err != nil {
		return math.MaxInt64
	}
	return t
}

// SumPreorderItems totals a booking's pre-ordered lines in minor units,
// excluding cancelled lines. This is the SINGLE definition of a booking's
// pre-order amount, shared by the guest-facing total (usecase/preorder) and the
// amount actually charged (usecase/payments.resolveAmount → PurposePreorder) so
// the displayed total and the charge can never drift apart. Overflow-guarded per
// line and across the running sum.
func SumPreorderItems(items []BookingItem) (int64, error) {
	var total int64
	for _, it := range items {
		if it.Status == BookingItemCancelled {
			continue
		}
		line, err := it.TotalMinorChecked()
		if err != nil {
			return 0, err
		}
		if line > math.MaxInt64-total {
			return 0, fmt.Errorf("%w: pre-order total overflows", ErrValidation)
		}
		total += line
	}
	return total, nil
}

// BookingItemRepository persists pre-ordered menu positions.
type BookingItemRepository interface {
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingItem, error)
	// ReplaceForBooking deletes the booking's items and inserts the given set
	// (call inside a TxManager).
	ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, items []BookingItem) error
	Create(ctx context.Context, items []BookingItem) error
	SetStatus(ctx context.Context, id uuid.UUID, status BookingItemStatus) error
}
