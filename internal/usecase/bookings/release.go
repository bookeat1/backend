package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Releaser hands a hidden pre-order booking to the venue. It is called by the
// payments webhook / reconciler THROUGH THE CONTEXT TRANSACTION of the payment's
// created -> authorized transition, so the venue learns about the booking in the
// very commit that makes its money real.
type Releaser interface {
	ReleaseForPayment(ctx context.Context, bookingID uuid.UUID) error
}

type releaser struct {
	bookings domain.BookingRepository
	release  domain.BookingReleaseRepository
	history  domain.BookingStatusHistoryRepository
	outbox   domain.BookingOutboxRepository
	rests    restaurantReader
	cfg      Config
}

// NewReleaser constructs the booking gate release.
func NewReleaser(
	bookings domain.BookingRepository,
	release domain.BookingReleaseRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	rests restaurantReader,
	cfg Config,
) Releaser {
	return &releaser{bookings: bookings, release: release, history: history, outbox: outbox, rests: rests, cfg: cfg.withDefaults()}
}

// ReleaseForPayment is idempotent: the conditional UPDATE reports false for an
// already-released (duplicate delivery) or no longer pending booking, and then
// nothing is written. For a venue with confirm_on_create the booking is
// confirmed in the same transaction; the caller captures the hold afterwards
// (captureHeldPreorderIfConfirmed).
func (r *releaser) ReleaseForPayment(ctx context.Context, bookingID uuid.UUID) error {
	now := time.Now()
	ok, err := r.release.Release(ctx, bookingID, now)
	if err != nil || !ok {
		return err
	}
	b, err := r.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return err
	}
	b.ReleasedToVenueAt = &now
	if err := publish(ctx, r.outbox, b, domain.EventBookingCreated, now); err != nil {
		return err
	}
	rest, err := r.rests.GetByID(ctx, b.RestaurantID)
	if err != nil {
		return err
	}
	if !resolvePolicy(rest.Restaurant, r.cfg).ConfirmOnCreate {
		return nil
	}
	from := b.Status
	b.Status, b.ConfirmedAt = domain.BookingConfirmed, &now
	if err := r.bookings.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, now); err != nil {
		return fmt.Errorf("confirm released booking: %w", err)
	}
	return recordTransition(ctx, r.history, r.outbox, b, &from, domain.ActorSystem, nil, strPtr("auto-confirm"), now)
}
