package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SenderType identifies who wrote a booking message, stored as VARCHAR.
type SenderType string

const (
	SenderGuest      SenderType = "guest"
	SenderRestaurant SenderType = "restaurant"
	SenderSystem     SenderType = "system"
)

// Valid reports whether s is a known sender type.
func (s SenderType) Valid() bool {
	switch s {
	case SenderGuest, SenderRestaurant, SenderSystem:
		return true
	}
	return false
}

// BookingMessage is one entry in the guest ↔ restaurant thread attached to a
// booking. SenderID is nil for system messages. ReadAt feeds the venue
// response-time metric.
type BookingMessage struct {
	ID         uuid.UUID
	BookingID  uuid.UUID
	SenderType SenderType
	SenderID   *uuid.UUID
	Message    string
	IsRead     bool
	ReadAt     *time.Time
	CreatedAt  time.Time
}

// BookingMessageRepository persists the booking chat.
type BookingMessageRepository interface {
	// ListByBooking returns the thread ordered by created_at ascending.
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingMessage, error)
	Create(ctx context.Context, m *BookingMessage) error
	// MarkRead flags unread messages of the booking not sent by reader as read
	// at the given time, and returns the number of affected rows.
	MarkRead(ctx context.Context, bookingID uuid.UUID, reader SenderType, at time.Time) (int, error)
}
