package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RefundStatus is the lifecycle state of a single refund, stored as VARCHAR.
// Refunds have their own, much shorter machine than payments: they are created,
// then they either land or they do not.
type RefundStatus string

const (
	RefundCreated   RefundStatus = "created"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// Valid reports whether s is a known refund status.
func (s RefundStatus) Valid() bool {
	switch s {
	case RefundCreated, RefundSucceeded, RefundFailed:
		return true
	}
	return false
}

// PaymentRefund is one refund against a payment. Refunds are partial and
// repeatable (a single pre-ordered dish may be refunded while the rest of the
// booking stands), which is why they are a table and not a column.
type PaymentRefund struct {
	ID               uuid.UUID
	PaymentID        uuid.UUID
	ProviderRefundID *string // nil until the acquirer answers
	AmountMinor      int64
	Currency         Currency
	Status           RefundStatus
	Reason           *string
	IdempotencyKey   string
	FailureCode      *string
	FailureMessage   *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Amount returns the refund amount as Money.
func (r PaymentRefund) Amount() Money {
	return Money{AmountMinor: r.AmountMinor, Currency: r.Currency}
}

// PaymentRefundRepository persists refunds. Get* return ErrNotFound when absent.
type PaymentRefundRepository interface {
	Create(ctx context.Context, r *PaymentRefund) error
	Update(ctx context.Context, r *PaymentRefund) error
	GetByID(ctx context.Context, id uuid.UUID) (*PaymentRefund, error)
	ListByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]PaymentRefund, error)
	// SucceededTotal is the sum of successful refunds for a payment. It is the
	// left-hand side of "never refund more than what is left" and must be read
	// inside the same transaction as the refund insert (spec §8).
	SucceededTotal(ctx context.Context, paymentID uuid.UUID) (int64, error)
}

// ---------------------------------------------------------------------------
// Refund settlement (spec §9.1, owner decision: variant A)
// ---------------------------------------------------------------------------

// RefundTrigger is what caused the money to be settled. It is a payment-domain
// type on purpose and is NOT the booking's CancelledBy: a no-show is not a
// cancellation, and the money consequences differ per trigger.
type RefundTrigger string

const (
	// RefundTriggerGuestCancel — the guest cancelled. Whether it happened
	// before or after the venue's cancellation deadline decides everything.
	RefundTriggerGuestCancel RefundTrigger = "guest_cancel"
	// RefundTriggerVenueCancel — the venue cancelled. The only case of a full
	// refund including the service fee: the guest must not pay for a service
	// they did not receive.
	RefundTriggerVenueCancel RefundTrigger = "venue_cancel"
	// RefundTriggerNoShow — the guest never came. Settled exactly like a late
	// cancellation: the table was held and other guests were turned away.
	RefundTriggerNoShow RefundTrigger = "no_show"
)

// Valid reports whether t is a known refund trigger.
func (t RefundTrigger) Valid() bool {
	switch t {
	case RefundTriggerGuestCancel, RefundTriggerVenueCancel, RefundTriggerNoShow:
		return true
	}
	return false
}

// RefundPolicy is the settlement configuration. Values come from env
// (PAYMENTS_REFUND_ACQUIRING_BPS) and may later be overridden per venue.
type RefundPolicy struct {
	// AcquiringBps is what the acquirer keeps when money travels back, in
	// basis points of the total charged (100 = 1%). Withheld from the guest's
	// refund; it is a cost, not platform revenue, so it lands on the
	// `acquirer` ledger account.
	AcquiringBps int
}

// Settlement is the breakdown of a captured payment across the four ledger
// accounts once the booking's outcome is known. The four parts always add up to
// the total charged — that is checked before this type is ever returned, and it
// is the same invariant the ledger asserts (ValidateLedgerBalance).
type Settlement struct {
	GuestMinor      int64 // refunded to the guest
	RestaurantMinor int64 // payable to the venue
	PlatformMinor   int64 // BookEat service fee kept
	AcquirerMinor   int64 // cost of moving the money back
	Currency        Currency
}

// Total returns the sum of all four parts.
func (s Settlement) Total() Money {
	return Money{
		AmountMinor: s.GuestMinor + s.RestaurantMinor + s.PlatformMinor + s.AcquirerMinor,
		Currency:    s.Currency,
	}
}

// SettleRefund splits a CAPTURED payment across guest / restaurant / platform /
// acquirer according to the owner's decision in spec §9.1 (variant A):
//
//	trigger / timing                 guest              restaurant  platform  acquirer
//	guest cancels before deadline    total − acquiring  0           0         acquiring
//	guest cancels after deadline     0                  base        fee       0
//	no-show                          0                  base        fee       0
//	venue cancels (any time)         total              0           0         0
//
// The logic behind variant A: a late cancellation is a no-show — the venue held
// the table and turned other guests away. A venue-caused cancellation is the one
// case of a full refund, service fee included.
//
// It is a pure function: no clock, no repository, no acquirer. cancelledAt and
// cancelDeadline are passed in so the decision is reproducible in a test and in
// a dispute.
//
// Preconditions: the payment must be captured (an authorized-but-not-captured
// payment is voided, not settled) and must not have been refunded already.
// Partial pre-order refunds (a single dish the venue cannot serve) are a
// different operation and do not go through here.
func SettleRefund(p Payment, trigger RefundTrigger, cancelledAt, cancelDeadline time.Time, policy RefundPolicy) (Settlement, error) {
	if !trigger.Valid() {
		return Settlement{}, fmt.Errorf("unknown refund trigger %q: %w", trigger, ErrValidation)
	}
	if p.Status != PaymentCaptured {
		return Settlement{}, fmt.Errorf("settle %s payment: %w", p.Status, ErrInvalidStatus)
	}
	if p.AmountMinor != p.BaseAmountMinor+p.FeeMinor {
		return Settlement{}, fmt.Errorf("payment amount %d != base %d + fee %d: %w",
			p.AmountMinor, p.BaseAmountMinor, p.FeeMinor, ErrValidation)
	}

	total := p.Total()
	s := Settlement{Currency: p.Currency}

	switch {
	case trigger == RefundTriggerVenueCancel:
		// The guest gets everything back, service fee included.
		s.GuestMinor = total.AmountMinor

	case trigger == RefundTriggerNoShow || !cancelledAt.Before(cancelDeadline):
		// Late cancellation is settled as a no-show: the base goes to the
		// venue, the platform keeps the service fee, nothing goes back.
		s.RestaurantMinor = p.BaseAmountMinor
		s.PlatformMinor = p.FeeMinor

	default:
		// Cancellation in time: everything back except the cost of moving the
		// money, rounded UP in the acquirer's favour (see ApplyBasisPoints).
		acquiring, err := ApplyBasisPoints(total, policy.AcquiringBps)
		if err != nil {
			return Settlement{}, err
		}
		refund, err := total.Sub(acquiring)
		if err != nil {
			return Settlement{}, err
		}
		s.GuestMinor = refund.AmountMinor
		s.AcquirerMinor = acquiring.AmountMinor
	}

	// Belt and braces: money is never created or destroyed here.
	if s.Total().AmountMinor != total.AmountMinor {
		return Settlement{}, fmt.Errorf("settlement %d != payment total %d: %w",
			s.Total().AmountMinor, total.AmountMinor, ErrValidation)
	}
	return s, nil
}
