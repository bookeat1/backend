package payments

import (
	"fmt"
	"strings"
	"time"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/payments"
)

// createPaymentRequest is the body of POST /bookings/{id}/payment.
//
// CallbackURL is deliberately NOT a field here: it is our own webhook route,
// built server-side (see callbackURL in handler.go). Accepting it from the
// client would let an attacker redirect the acquirer's webhook delivery
// somewhere else entirely.
type createPaymentRequest struct {
	ReturnURL string `json:"return_url"`
}

func (r createPaymentRequest) validate() error {
	if strings.TrimSpace(r.ReturnURL) == "" {
		return fmt.Errorf("%w: return_url is required", domain.ErrValidation)
	}
	return nil
}

// settleRequest is the body of POST /bookings/{id}/payment/settle. The
// idempotency key comes from the header, same convention as
// createPaymentRequest / bookings' Idempotency-Key.
type settleRequest struct {
	Trigger string  `json:"trigger"`
	Reason  *string `json:"reason"`
	// ManualCancelledAt overrides the booking's own recorded cancellation time.
	// STAFF/ADMIN ONLY — the usecase itself rejects it from any other actor
	// (SettleInput.ManualCancelledAt's doc comment); accepting the field here
	// and letting the usecase reject it is simpler and no less safe than
	// trying to strip it in the handler before the actor's role is known.
	ManualCancelledAt *time.Time `json:"manual_cancelled_at"`
}

func (r settleRequest) toInput(idempotencyKey string) (uc.SettleInput, error) {
	trigger := domain.RefundTrigger(strings.TrimSpace(r.Trigger))
	if !trigger.Valid() {
		return uc.SettleInput{}, fmt.Errorf("%w: unknown trigger %q", domain.ErrValidation, r.Trigger)
	}
	return uc.SettleInput{
		Trigger: trigger, Reason: r.Reason, ManualCancelledAt: r.ManualCancelledAt,
		IdempotencyKey: idempotencyKey,
	}, nil
}

// voidRequest is the (optional) body of POST /bookings/{id}/payment/void.
type voidRequest struct {
	Reason *string `json:"reason"`
}

func (r voidRequest) reasonOrDefault() string {
	if r.Reason != nil && strings.TrimSpace(*r.Reason) != "" {
		return *r.Reason
	}
	return "rejected by venue"
}
