package tickets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/payments"
)

// RefundUseCase cancels a PAID ticket: it refunds the guest via the payments
// refund flow and returns the held capacity by moving the ticket to refunded.
type RefundUseCase interface {
	// Refund refunds a paid ticket in full and frees its capacity. Authorization
	// (staff PermPaymentRefund at the event's restaurant, or the ticket's own
	// buyer) is enforced by the payments refund it delegates to. Idempotent: a
	// retry with the same key resolves to the already-refunded ticket.
	Refund(ctx context.Context, actor Actor, ticketID uuid.UUID, in RefundInput) (*domain.EventTicket, error)
}

// RefundInput carries the refund reason and idempotency key.
type RefundInput struct {
	Reason         *string
	IdempotencyKey string
}

type refundUseCase struct {
	tickets  domain.EventTicketRepository
	payments ticketPayments
}

// NewRefundUseCase constructs the ticket-refund usecase.
func NewRefundUseCase(tickets domain.EventTicketRepository, pay ticketPayments) RefundUseCase {
	return &refundUseCase{tickets: tickets, payments: pay}
}

// Refund resolves the ticket, refunds its payment (money side, authorized
// there), then moves the ticket paid→refunded to free the seat. The money move
// happens BEFORE the ticket status change; if the refund succeeds but the
// status CAS races a concurrent identical refund, the already-refunded ticket
// is returned (idempotent, capacity freed once).
func (u *refundUseCase) Refund(ctx context.Context, actor Actor, ticketID uuid.UUID, in RefundInput) (*domain.EventTicket, error) {
	t, err := u.tickets.GetByID(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	if t.Status == domain.TicketRefunded {
		return t, nil // already refunded — idempotent
	}
	if t.Status != domain.TicketPaid {
		return nil, fmt.Errorf("%w: ticket is %s, only a paid ticket can be refunded", domain.ErrInvalidStatus, t.Status)
	}
	if t.PaymentID == nil {
		return nil, fmt.Errorf("%w: ticket %s has no payment to refund", domain.ErrValidation, t.ID)
	}

	if _, err := u.payments.RefundTicket(ctx, actor.payment(), payments.TicketRefundInput{
		PaymentID:      *t.PaymentID,
		OwnerUserID:    t.UserID,
		Reason:         in.Reason,
		IdempotencyKey: in.IdempotencyKey,
	}); err != nil {
		return nil, err
	}

	if err := u.tickets.CompareAndSwapStatus(ctx, t.ID, domain.TicketPaid, domain.TicketRefunded, time.Now()); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			// A concurrent identical refund already moved the ticket. Re-read.
			current, gerr := u.tickets.GetByID(ctx, t.ID)
			if gerr != nil {
				return nil, gerr
			}
			return current, nil
		}
		return nil, err
	}
	t.Status = domain.TicketRefunded
	return t, nil
}
