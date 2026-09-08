package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ExternalSource records where an occupancy hold that did NOT originate as a
// BookEat booking came from. BookEat is only one funnel: a table can be taken
// by a phone call, a walk-in, or an external POS / booking system, and the
// availability engine must not resell a slot already taken elsewhere. Stored as
// VARCHAR, validated here — never a Postgres ENUM.
type ExternalSource string

const (
	// ExtSourceManual is a hold a staff member typed in by hand.
	ExtSourceManual ExternalSource = "manual"
	// ExtSourcePhone is a reservation taken over the phone.
	ExtSourcePhone ExternalSource = "phone"
	// ExtSourceWalkin is a party seated on arrival without a prior booking.
	ExtSourceWalkin ExternalSource = "walkin"
	// ExtSourcePOS is occupancy pushed by a generic external POS / booking system.
	ExtSourcePOS ExternalSource = "pos"
	// ExtSourceKwaaka is reserved for the planned Kwaaka integration: the webhook
	// POST /hooks/{restaurantId}/reserve/status writes holds with this source.
	// No live Kwaaka client exists yet — this is the ingestion seam, not a client.
	ExtSourceKwaaka ExternalSource = "kwaaka"
)

// Valid reports whether s is a known external source.
func (s ExternalSource) Valid() bool {
	switch s {
	case ExtSourceManual, ExtSourcePhone, ExtSourceWalkin, ExtSourcePOS, ExtSourceKwaaka:
		return true
	}
	return false
}

// ExternalReservation is occupancy that originated outside BookEat and feeds the
// same availability engine as a native booking. It is source-agnostic on
// purpose: a manual phone entry, a walk-in and a future POS/Kwaaka webhook all
// produce the same record.
//
// TableID nil = a whole-venue block (private event, kitchen closed). Such a
// block is expanded to one booking_tables enforcement row per active table at
// creation time (see usecase/bookings.ExternalReservationUseCase), so every
// table that exists at that moment is protected by the same GiST exclusion
// constraint that guards native bookings.
//
// StartsAt/EndsAt is the raw occupied window as reported by the other channel —
// NOT widened by BookEat's cleanup buffer. The caller owns its own timing; the
// buffer belongs to bookings this system creates, not to what another system
// already scheduled.
type ExternalReservation struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	TableID      *uuid.UUID
	StartsAt     time.Time
	EndsAt       time.Time
	Source       ExternalSource
	ExternalRef  *string
	Note         *string
	CreatedBy    *uuid.UUID
	Active       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ExternalReservationRepository persists external occupancy holds. The physical
// double-booking guarantee is the GiST exclusion constraint on booking_tables
// (into which Create writes one enforcement row per occupied table), not any
// application-level check — a check loses the race, a constraint does not.
type ExternalReservationRepository interface {
	// Create inserts the record and one booking_tables enforcement row per
	// enforceTableIDs, in the same transaction (call inside a TxManager).
	// Returns ErrAlreadyExists when any enforced slot overlaps an active booking
	// or an active hold. enforceTableIDs may be empty (a whole-venue block at a
	// venue that currently has no active tables): the record is still stored.
	Create(ctx context.Context, r *ExternalReservation, enforceTableIDs []uuid.UUID) error
	// GetByID returns ErrNotFound when absent.
	GetByID(ctx context.Context, id uuid.UUID) (*ExternalReservation, error)
	// Delete removes the hold; its booking_tables enforcement rows cascade away,
	// freeing the slot. Returns ErrNotFound when absent.
	Delete(ctx context.Context, id uuid.UUID) error
	// List returns the active holds of a restaurant overlapping [from, to),
	// ordered by start time.
	List(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]ExternalReservation, error)
}
