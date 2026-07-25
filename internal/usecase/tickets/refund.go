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
	// WHO the caller is comes first — before the idempotent return, before any
	// status branch. Everything after this point either moves money or hands
	// back the ticket, which carries the buyer's name, phone and email; a bare
	// ticket id is not proof of anything (that is the whole premise of this
	// endpoint), so answering an unauthorized caller at all — even with a
	// friendly "already refunded" — would leak a stranger's contacts and let
	// anyone probe ticket ids for their status.
	staff, err := u.authorizeCaller(ctx, actor, t)
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

	// The POLICY question is only meaningful for a ticket that is still paid,
	// so it stays here, after the status branches.
	if err := u.enforceRefundPolicy(ctx, staff, t, in); err != nil {
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

// authorizeCaller answers only "may this caller act on this ticket at all",
// with no reference to the refund policy or to the ticket's status, so it can
// run first. It reports whether the caller is venue staff, which decides which
// rules enforceRefundPolicy then holds them to.
func (u *refundUseCase) authorizeCaller(ctx context.Context, actor Actor, t *domain.EventTicket) (bool, error) {
	staff, err := u.isRefundStaff(ctx, actor, t.RestaurantID)
	if err != nil {
		return false, err
	}
	if staff {
		return true, nil
	}

	// Guest path. Ownership must be PROVEN, not assumed: a ticket bought under
	// an account is refundable only by that account. An account-less (guest
	// checkout) ticket carries no proof of ownership at this endpoint — the
	// ticket id alone is not one — so it has no self-refund path at all and must
	// go through the venue. Better a support request than a stranger cancelling
	// somebody else's ticket.
	if t.UserID == nil {
		return false, fmt.Errorf("%w: a ticket bought without an account can only be refunded by the venue", domain.ErrForbidden)
	}
	if actor.UserID == nil || *actor.UserID != *t.UserID {
		return false, fmt.Errorf("%w: this ticket belongs to another guest", domain.ErrForbidden)
	}
	return false, nil
}

// enforceRefundPolicy holds the caller to the policy the ticket was SOLD under.
// Staff are the exception path and may go outside the window, but only with an
// explicit override — without it they are held to the same policy as the guest,
// so a "refund this" click in the cabinet cannot quietly become a policy waiver.
func (u *refundUseCase) enforceRefundPolicy(ctx context.Context, staff bool, t *domain.EventTicket, in RefundInput) error {
	decision, err := u.decide(ctx, t)
	if err != nil {
		return err
	}
	if decision.Allowed {
		return nil
	}
	if staff {
		if in.Override {
			return nil
		}
		return fmt.Errorf("%w: this event's refund policy refuses this refund (%s) — repeat with an explicit override to refund anyway",
			domain.ErrForbidden, decision.Reason)
	}
	if decision.Reason == domain.TicketRefundDenyNotRefundable {
		return fmt.Errorf("%w: tickets for this event are non-refundable", domain.ErrForbidden)
	}
	return fmt.Errorf("%w: the refund window for this event closed at %s",
		domain.ErrForbidden, decision.Deadline.Format(time.RFC3339))
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
