package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ActorType identifies who caused a status change, stored as VARCHAR.
type ActorType string

const (
	ActorGuest   ActorType = "guest"
	ActorManager ActorType = "manager"
	ActorAdmin   ActorType = "admin"
	ActorSystem  ActorType = "system"
)

// Valid reports whether a is a known actor type.
func (a ActorType) Valid() bool {
	switch a {
	case ActorGuest, ActorManager, ActorAdmin, ActorSystem:
		return true
	}
	return false
}

// BookingStatusChange is one row of a booking's status audit trail. FromStatus
// is nil for the initial record. Written in the same transaction as the
// bookings UPDATE it describes.
type BookingStatusChange struct {
	ID         uuid.UUID
	BookingID  uuid.UUID
	FromStatus *BookingStatus
	ToStatus   BookingStatus
	ActorType  ActorType
	ActorID    *uuid.UUID
	Reason     *string
	CreatedAt  time.Time
}

// BookingStatusHistoryRepository persists the status audit trail.
type BookingStatusHistoryRepository interface {
	Create(ctx context.Context, c *BookingStatusChange) error
	// ListByBooking returns the trail ordered by created_at ascending.
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]BookingStatusChange, error)
}
