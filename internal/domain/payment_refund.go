package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RefundStatus is the lifecycle state of a single refund, stored as VARCHAR.
//
//	created ──claim──▶ in_flight ──acquirer call──┬──▶ succeeded  (explicit OK)
//	                                                ├──▶ failed     (explicit decline, a code came back)
//	                                                └──▶ pending    (timeout / 5xx / malformed — outcome UNKNOWN)
//
// `created` and `in_flight` exist to make the acquirer call itself race-safe
// (report item #2): two concurrent Settle calls for the SAME idempotency key
// both insert-or-find the same `created` row, but only the one that wins the
// created→in_flight CAS may call the acquirer — the loser gets ErrAlreadyExists
// and must not call it a second time.
//
// `pending` exists because a network timeout or a 5xx is NOT the same fact as
// an explicit decline (report item #1): retrying the acquirer call for a
// `pending` refund would be retrying an operation whose first attempt may have
// already succeeded at the provider, i.e. a second real-world refund. A refund
// in `pending` may only be resolved by reading the acquirer's own state
// (PaymentGateway.Get) or by a future webhook — never by calling Refund again.
type RefundStatus string

const (
	RefundCreated   RefundStatus = "created"
	RefundInFlight  RefundStatus = "in_flight"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	// RefundPending is the uncertain-outcome state: the acquirer call ended in
	// a timeout, a 5xx, or an unparsable answer. See the type doc comment.
	RefundPending RefundStatus = "pending"
)

// Valid reports whether s is a known refund status.
func (s RefundStatus) Valid() bool {
	switch s {
	case RefundCreated, RefundInFlight, RefundSucceeded, RefundFailed, RefundPending:
		return true
	}
	return false
}

// RetryableAtAcquirer reports whether the caller may call PaymentGateway.Refund
// again for a refund in this status. Only `created` may retry-from-scratch (it
// never reached the acquirer); `in_flight` is reserved for whoever won the CAS
// currently making the call; `pending` explicitly may NOT retry (report item
// #1) — it can only be resolved by reading the acquirer's own status.
func (s RefundStatus) RetryableAtAcquirer() bool {
	return s == RefundCreated
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
	// StatusChangedAt is the reconciliation lease clock, same purpose as
	// Payment.StatusChangedAt (migration 0010): stamped every time Status
	// actually changes, so "how long has this refund been in_flight/pending"
	// can be measured without confusing it with CreatedAt.
	StatusChangedAt time.Time
	// ReconcileAttempts / LastReconcileAttemptAt / NeedsManualReview mirror
	// Payment's fields of the same name, applied to one refund attempt.
	ReconcileAttempts      int
	LastReconcileAttemptAt *time.Time
	NeedsManualReview      bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
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
	// GetByIdempotencyKey resolves our own retry token for one payment, backed
	// by idx_payment_refunds_idempotency (UNIQUE (payment_id, idempotency_key)).
	// A retry of the same cancellation/settlement request replays this row
	// instead of asking the acquirer to refund twice. Returns ErrNotFound when
	// unused.
	GetByIdempotencyKey(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*PaymentRefund, error)
	ListByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]PaymentRefund, error)
	// SucceededTotal is the sum of successful refunds for a payment. It is the
	// left-hand side of "never refund more than what is left" and must be read
	// inside the same transaction as the refund insert (spec §8).
	SucceededTotal(ctx context.Context, paymentID uuid.UUID) (int64, error)
	// CompareAndSwapStatus is the same DB-level guard as
	// PaymentRepository.CompareAndSwapStatus, applied to one refund row: a
	// single `UPDATE payment_refunds SET status = $to, updated_at = $at WHERE
	// id = $id AND status = $from`. It returns ErrAlreadyExists when zero rows
	// matched. This is what makes the created→in_flight claim exclusive
	// (report item #2): the loser of a concurrent Settle race for the same
	// idempotency key must never reach the acquirer call.
	CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to RefundStatus, at time.Time) error
	// ClaimStale selects up to limit refunds in the given statuses whose
	// StatusChangedAt is older than before, oldest first — the reconciliation
	// worker's input for a refund left in_flight/pending after a crash or a
	// timeout (usecase/payments.Reconciler). Same non-locking-across-the-
	// acquirer-call caveat as PaymentRepository.ClaimStale.
	ClaimStale(ctx context.Context, statuses []RefundStatus, before time.Time, limit int) ([]PaymentRefund, error)
	// RecordReconcileAttempt is the CAS-guarded write behind
	// ReconcileAttempts / LastReconcileAttemptAt / NeedsManualReview, same
	// contract as PaymentRepository.RecordReconcileAttempt: `UPDATE
	// payment_refunds SET reconcile_attempts = reconcile_attempts + 1,
	// last_reconcile_attempt_at = $at,
	// needs_manual_review = (reconcile_attempts + 1 >= $maxAttempts) WHERE
	// id = $id AND status = $expectedStatus RETURNING reconcile_attempts,
	// needs_manual_review`. ErrAlreadyExists means the refund's status
	// already moved on — nothing to bump.
	RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus RefundStatus, at time.Time, maxAttempts int) (attempts int, needsManualReview bool, err error)
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
