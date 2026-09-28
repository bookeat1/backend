package bookings

import (
	"context"
	"time"

	"backend-core/internal/domain"
)

// VenueAnswerDeadlineResolver computes venue_answer_deadline_at for one booking
// (spec criterion 24), reusing the SAME restaurant policy reader and
// PreorderHoldChecker the confirm-SLA worker uses, over the SAME
// domain.VenueAnswerDeadline formula — so the deadline shown to the guest can
// never disagree with the one the worker actually enforces (spec §3 D_venue).
//
// Bound in bootstrap to Facade via WithVenueAnswerDeadlineResolver; left nil in
// tests / when payments are not wired, in which case the field is omitted.
type VenueAnswerDeadlineResolver struct {
	restaurants restaurantReader
	holds       PreorderHoldChecker
	cfg         Config
	confirmMax  time.Duration
}

// NewVenueAnswerDeadlineResolver builds the resolver. confirmMax is
// PAYMENTS_PREORDER_CONFIRM_MAX (bootstrap Config.Payments.PreorderConfirmMax),
// the SAME value WorkerConfig.ConfirmMax is built from.
func NewVenueAnswerDeadlineResolver(
	restaurants restaurantReader,
	holds PreorderHoldChecker,
	cfg Config,
	confirmMax time.Duration,
) *VenueAnswerDeadlineResolver {
	return &VenueAnswerDeadlineResolver{
		restaurants: restaurants, holds: holds, cfg: cfg.withDefaults(), confirmMax: confirmMax,
	}
}

// VenueAnswerDeadline returns nil when the deadline does not apply: the
// booking is not `pending`, was never released to the venue (still hidden —
// its own D_pay applies instead, not D_venue), or carries no live pre-order
// hold (an ordinary booking with no money on hold has no forced-cancel
// deadline, only the softer auto-confirm/escalate SLA).
func (r *VenueAnswerDeadlineResolver) VenueAnswerDeadline(ctx context.Context, b domain.Booking) (*time.Time, error) {
	if b.Status != domain.BookingPending || b.ReleasedToVenueAt == nil {
		return nil, nil
	}
	held, err := r.holds.HasPreorderHold(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	rest, err := r.restaurants.GetByID(ctx, b.RestaurantID)
	if err != nil {
		return nil, err
	}
	policy := resolvePolicy(rest.Restaurant, r.cfg)
	d := domain.VenueAnswerDeadline(*b.ReleasedToVenueAt, b.StartsAt, policy.ConfirmSLA, r.confirmMax)
	return &d, nil
}
