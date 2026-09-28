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
	// holds and confirmMax drive the venue "booking created" money+deadline
	// line (spec preorder-hold-capture-on-confirm-20260924 §criterion 25); both
	// nil/zero = the line is simply omitted, same degrade-safe shape as every
	// other WithReleaser* option below.
	holds      PreorderHoldNotice
	confirmMax time.Duration
}

// NewReleaser constructs the booking gate release.
func NewReleaser(
	bookings domain.BookingRepository,
	release domain.BookingReleaseRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	rests restaurantReader,
	cfg Config,
	opts ...ReleaserOption,
) Releaser {
	r := &releaser{bookings: bookings, release: release, history: history, outbox: outbox, rests: rests, cfg: cfg.withDefaults()}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ReleaserOption configures optional Releaser dependencies without breaking
// the constructor's existing positional callers.
type ReleaserOption func(*releaser)

// PreorderHoldNotice reports the amount of a booking's live pre-order hold
// (authorized, not yet captured), so the venue's "new booking" notification
// can say how much is blocked. Mirrors PreorderHoldChecker's data source
// (domain.PaymentRepository.GetLiveByBookingID) but returns the amount too,
// since the worker's checker never needs to print money, only decide
// auto-confirm vs escalate.
type PreorderHoldNotice interface {
	LivePreorderHold(ctx context.Context, bookingID uuid.UUID) (domain.Money, bool, error)
}

// WithReleaserVenueNotice wires the pre-order hold reader and the SAME
// PAYMENTS_PREORDER_CONFIRM_MAX the confirm-SLA worker and
// VenueAnswerDeadlineResolver use, so the deadline quoted to the venue at
// release time can never disagree with the one the worker actually enforces.
func WithReleaserVenueNotice(holds PreorderHoldNotice, confirmMax time.Duration) ReleaserOption {
	return func(r *releaser) { r.holds, r.confirmMax = holds, confirmMax }
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
	rest, err := r.rests.GetByID(ctx, b.RestaurantID)
	if err != nil {
		return err
	}
	policy := resolvePolicy(rest.Restaurant, r.cfg)

	var opts []payloadOption
	// The money+deadline line applies only to a booking the venue must still
	// ANSWER: one auto-confirmed in this same call needs no deadline (spec
	// VenueAnswerDeadlineResolver has the identical guard for the read side).
	if !policy.ConfirmOnCreate && r.holds != nil {
		amount, held, herr := r.holds.LivePreorderHold(ctx, bookingID)
		if herr != nil {
			return herr
		}
		if held {
			deadline := domain.VenueAnswerDeadline(now, b.StartsAt, policy.ConfirmSLA, r.confirmMax)
			opts = append(opts, withHoldNotice(amount, deadline))
		}
	}
	if err := publish(ctx, r.outbox, b, domain.EventBookingCreated, now, opts...); err != nil {
		return err
	}
	if !policy.ConfirmOnCreate {
		return nil
	}
	from := b.Status
	b.Status, b.ConfirmedAt = domain.BookingConfirmed, &now
	if err := r.bookings.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, now); err != nil {
		return fmt.Errorf("confirm released booking: %w", err)
	}
	return recordTransition(ctx, r.history, r.outbox, b, &from, domain.ActorSystem, nil, strPtr("auto-confirm"), now)
}
