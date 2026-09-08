package tickets

import (
	"context"
	"errors"
	"time"

	"backend-core/internal/domain"
)

// PaymentObserver projects a payment's status onto its event ticket. It
// implements payments.PaymentSubjectObserver and is invoked by the payments
// webhook AFTER a status change commits, for ticket payments only. This is what
// makes "payment success → ticket paid" and "payment failure → capacity
// released" happen through the SAME webhook flow bookings use, without the
// payments package knowing anything about tickets.
//
// Capacity is a projection over ticket status (SUM of pending+paid), so a
// released seat is freed simply by moving the ticket out of those statuses —
// there is no counter to decrement and therefore no drift.
type PaymentObserver struct {
	tickets domain.EventTicketRepository
}

// NewPaymentObserver builds the payment→ticket projection.
func NewPaymentObserver(tickets domain.EventTicketRepository) *PaymentObserver {
	return &PaymentObserver{tickets: tickets}
}

// OnPaymentApplied maps p's current payment status onto its ticket:
//   - captured                    → pending → paid       (holds capacity)
//   - failed / expired / voided   → pending → cancelled  (frees capacity)
//   - refunded                    → paid    → refunded    (frees capacity)
//
// Any other status (created/authorized/capturing/voiding) is a no-op: the
// ticket stays pending. It is idempotent: a redelivered webhook whose ticket
// already reached the target status loses the CAS with ErrAlreadyExists, which
// is treated as success (the projection already happened), never a double move.
func (o *PaymentObserver) OnPaymentApplied(ctx context.Context, p *domain.Payment) error {
	if p.EventTicketID == nil {
		return nil
	}
	to, from, act := ticketTargetFor(p.Status)
	if !act {
		return nil
	}
	err := o.tickets.CompareAndSwapStatus(ctx, *p.EventTicketID, from, to, time.Now())
	if err == nil || errors.Is(err, domain.ErrAlreadyExists) {
		// ErrAlreadyExists: the ticket already left `from` (a redelivery, or it
		// is already terminal) — idempotent, nothing to do.
		return nil
	}
	return err
}

// ticketTargetFor maps a payment status to the (target, expected-from) ticket
// transition it drives, and whether any transition applies at all.
func ticketTargetFor(s domain.PaymentStatus) (to, from domain.TicketStatus, act bool) {
	switch s {
	case domain.PaymentCaptured:
		return domain.TicketPaid, domain.TicketPending, true
	case domain.PaymentFailed, domain.PaymentExpired, domain.PaymentVoided:
		return domain.TicketCancelled, domain.TicketPending, true
	case domain.PaymentRefunded:
		return domain.TicketRefunded, domain.TicketPaid, true
	default:
		return "", "", false
	}
}
