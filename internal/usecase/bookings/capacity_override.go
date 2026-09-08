package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// CapacityOverrideUseCase is the venue cabinet's read side of the deliberate
// overbookings (migration 0056). It exists so the audit trail is answerable by
// the venue owner rather than only by whoever has a psql prompt: "who put 12
// people in a 10-seat room last Saturday" is a product question.
//
// Read-only by construction — the rows are append-only in the database and this
// package offers no way to write one except as part of creating the booking.
type CapacityOverrideUseCase interface {
	// List returns the venue's overbookings whose overloaded moment lies in
	// [from, to), newest first.
	List(ctx context.Context, actor Actor, restaurantID uuid.UUID, from, to time.Time) ([]domain.BookingCapacityOverride, error)
}

// maxOverrideWindowDays bounds the query the same way the external-hold listing
// is bounded: this is a cabinet view over a chosen period, not an unbounded
// export of the venue's whole history.
const maxOverrideWindowDays = 92

type capacityOverrideUseCase struct {
	capacity domain.BookingCapacityRepository
	managers managerChecker
}

// NewCapacityOverrideUseCase constructs the overbooking audit reader.
func NewCapacityOverrideUseCase(capacity domain.BookingCapacityRepository, managers managerChecker) CapacityOverrideUseCase {
	return &capacityOverrideUseCase{capacity: capacity, managers: managers}
}

func (u *capacityOverrideUseCase) List(ctx context.Context, actor Actor, restaurantID uuid.UUID, from, to time.Time) ([]domain.BookingCapacityOverride, error) {
	// Same gate as the rest of the venue cabinet: the restaurant's own manager
	// or an admin. A manager of another venue gets 403.
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	if u.capacity == nil {
		return nil, fmt.Errorf("%w: capacity bookings are not configured", domain.ErrValidation)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%w: to must be after from", domain.ErrValidation)
	}
	if to.Sub(from) > maxOverrideWindowDays*24*time.Hour {
		return nil, fmt.Errorf("%w: the window must not exceed %d days", domain.ErrValidation, maxOverrideWindowDays)
	}
	return u.capacity.ListOverrides(ctx, restaurantID, from.UTC(), to.UTC())
}
