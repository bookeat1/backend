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
	// Refund refunds a paid ticket in full and frees its capacity. Two paths
	// exist and they are deliberately different:
	//   - the GUEST who bought the ticket may self-refund it, but only while the
	//     event's refund policy AS FROZEN ON THE TICKET allows it
	//     (domain.TicketRefundAllowed);
	//   - venue staff holding PermPaymentRefund at the event's restaurant may
	//     refund outside that window, but only by asking for it explicitly
	//     (RefundInput.Override) — a venue must be able to make an exception,
	//     and that exception must never happen by accident.
	// The money side is authorized again by the payments refund it delegates to.
	// Idempotent: a retry with the same key resolves to the already-refunded
	// ticket.
	Refund(ctx context.Context, actor Actor, ticketID uuid.UUID, in RefundInput) (*domain.EventTicket, error)
}

// RefundInput carries the refund reason and idempotency key.
type RefundInput struct {
	Reason         *string
	IdempotencyKey string
	// Override is the venue's explicit "refund this anyway". It is honoured ONLY
	// for staff holding PermPaymentRefund and only matters when the event's
	// policy would refuse the refund; for a guest it is ignored.
	Override bool
}

type refundUseCase struct {
	tickets  domain.EventTicketRepository
	events   eventReader
	payments ticketPayments
	perms    permissionChecker
}

// NewRefundUseCase constructs the ticket-refund usecase.
func NewRefundUseCase(tickets domain.EventTicketRepository, events eventReader, pay ticketPayments, perms permissionChecker) RefundUseCase {
	return &refundUseCase{tickets: tickets, events: events, payments: pay, perms: perms}
}

// Refund resolves the ticket, decides eligibility (policy for a guest, explicit
// override for authorized staff), refunds its payment (money side, authorized
// there too), then moves the ticket paid→refunded to free the seat. The money
// move happens BEFORE the ticket status change; if the refund succeeds but the
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

	if err := u.authorizeRefund(ctx, actor, t, in); err != nil {
		return nil, err
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

// authorizeRefund splits the two paths. Staff first: a caller with the venue's
// refund permission is the exception path and may go outside the window, but
// only with an explicit override — without it they are held to the same policy
// as the guest, so a "refund this" click in the cabinet cannot quietly become a
// policy waiver. Everyone else is the buyer, and the buyer is bound by the
// policy the ticket was SOLD under.
func (u *refundUseCase) authorizeRefund(ctx context.Context, actor Actor, t *domain.EventTicket, in RefundInput) error {
	decision, err := u.decide(ctx, t)
	if err != nil {
		return err
	}

	staff, err := u.isRefundStaff(ctx, actor, t.RestaurantID)
	if err != nil {
		return err
	}
	if staff {
		if !decision.Allowed && !in.Override {
			return fmt.Errorf("%w: this event's refund policy refuses this refund (%s) — repeat with an explicit override to refund anyway",
				domain.ErrForbidden, decision.Reason)
		}
		return nil
	}

	// Guest path. Ownership must be PROVEN, not assumed: a ticket bought under
	// an account is refundable only by that account. An account-less (guest
	// checkout) ticket carries no proof of ownership at this endpoint — the
	// ticket id alone is not one — so it has no self-refund path at all and must
	// go through the venue. Better a support request than a stranger cancelling
	// somebody else's ticket.
	if t.UserID == nil {
		return fmt.Errorf("%w: a ticket bought without an account can only be refunded by the venue", domain.ErrForbidden)
	}
	if actor.UserID == nil || *actor.UserID != *t.UserID {
		return fmt.Errorf("%w: this ticket belongs to another guest", domain.ErrForbidden)
	}
	if !decision.Allowed {
		if decision.Reason == domain.TicketRefundDenyNotRefundable {
			return fmt.Errorf("%w: tickets for this event are non-refundable", domain.ErrForbidden)
		}
		return fmt.Errorf("%w: the refund window for this event closed at %s",
			domain.ErrForbidden, decision.Deadline.Format(time.RFC3339))
	}
	return nil
}

// decide evaluates the ticket's OWN frozen policy against the event's start.
// Only the start time comes from the event — the rules come from the ticket, so
// a venue that tightens its policy after the sale cannot reach a ticket that is
// already bought.
func (u *refundUseCase) decide(ctx context.Context, t *domain.EventTicket) (domain.TicketRefundDecision, error) {
	event, err := u.events.GetByID(ctx, t.EventID)
	if err != nil {
		return domain.TicketRefundDecision{}, err
	}
	return domain.TicketRefundAllowed(t.RefundPolicy(), event.StartsAt, time.Now()), nil
}

// isRefundStaff reports whether the actor is venue staff holding
// PermPaymentRefund at the ticket's own restaurant (a superadmin bypasses, same
// contract as every other HasPermission call site). A staff-role actor that
// does NOT hold the permission at this restaurant is not staff for this
// decision — it falls through to the guest path, where it is rejected as a
// non-owner, so a manager of venue A can never refund venue B's ticket.
func (u *refundUseCase) isRefundStaff(ctx context.Context, actor Actor, restaurantID uuid.UUID) (bool, error) {
	if actor.Role == domain.RoleAdmin {
		return true, nil
	}
	if !actor.staff() || actor.UserID == nil {
		return false, nil
	}
	ok, err := u.perms.HasPermission(ctx, *actor.UserID, restaurantID, domain.PermPaymentRefund)
	if err != nil {
		return false, err
	}
	return ok, nil
}
