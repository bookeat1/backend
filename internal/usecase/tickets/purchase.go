package tickets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/payments"
)

// PurchaseUseCase sells tickets for a ticketed event: it validates the event,
// reserves capacity race-safely, then creates the payment.
type PurchaseUseCase interface {
	Purchase(ctx context.Context, actor Actor, in PurchaseInput) (*PurchaseResult, error)
}

// PurchaseInput is a buy request. The price is NEVER taken from here — it is
// read from the event (frozen onto the ticket), so a tampered client amount can
// never be charged.
type PurchaseInput struct {
	EventID        uuid.UUID
	Quantity       int
	GuestName      string
	GuestPhone     string // normalised by the transport layer
	GuestEmail     string
	IdempotencyKey string
	ReturnURL      string
	CallbackURL    string
}

// PurchaseResult is the ticket plus its payment. Payment.PaymentURL (when set)
// is where the guest is redirected to complete the hosted card payment; the
// ticket flips pending→paid once the acquirer confirms capture (webhook).
type PurchaseResult struct {
	Ticket  *domain.EventTicket
	Payment *domain.Payment
}

type purchaseUseCase struct {
	tickets     domain.EventTicketRepository
	events      eventReader
	payments    ticketPayments
	paymentRead paymentReader
	tx          domain.TxManager
}

// NewPurchaseUseCase constructs the ticket-purchase usecase.
func NewPurchaseUseCase(
	tickets domain.EventTicketRepository,
	events eventReader,
	pay ticketPayments,
	paymentRead paymentReader,
	tx domain.TxManager,
) PurchaseUseCase {
	return &purchaseUseCase{tickets: tickets, events: events, payments: pay, paymentRead: paymentRead, tx: tx}
}

func (u *purchaseUseCase) paymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	return u.paymentRead.GetByID(ctx, id)
}

// maxTicketsPerPurchase bounds a single purchase so one request cannot lock up
// an entire event's capacity (or overflow int arithmetic).
const maxTicketsPerPurchase = 50

// Purchase validates → reserves capacity → pays. The capacity reservation and
// the payment are two separate steps because the acquirer call must never run
// inside a DB transaction:
//
//  1. reserve: inside ONE tx, lock the event row (FOR UPDATE), read the sold
//     count, reject if it would oversell, and insert the pending ticket. The
//     row lock serialises concurrent buyers of the same event, so the last-seat
//     race resolves to exactly one winner (no oversell). The pending ticket
//     HOLDS the seat while payment is in flight.
//  2. pay: OUTSIDE the tx, create the payment (hold, grossed up). If it fails,
//     RELEASE the reservation (pending→cancelled) so the held seat is freed.
//
// Idempotent: a retry with the same key replays the existing ticket + payment
// instead of reserving a second batch of seats.
func (u *purchaseUseCase) Purchase(ctx context.Context, actor Actor, in PurchaseInput) (*PurchaseResult, error) {
	if in.Quantity <= 0 {
		return nil, fmt.Errorf("%w: quantity must be positive", domain.ErrValidation)
	}
	if in.Quantity > maxTicketsPerPurchase {
		return nil, fmt.Errorf("%w: at most %d tickets per purchase", domain.ErrValidation, maxTicketsPerPurchase)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}
	// An anonymous buyer MUST supply a phone: it is the contact for the ticket
	// and the ONLY ownership proof an idempotency replay can check (an account
	// buyer is proven by their user id instead). Without it a replay could not
	// be safely scoped to its buyer.
	if actor.UserID == nil && strings.TrimSpace(in.GuestPhone) == "" {
		return nil, fmt.Errorf("%w: a guest phone is required to buy a ticket without an account", domain.ErrValidation)
	}

	event, err := u.events.GetByID(ctx, in.EventID)
	if err != nil {
		return nil, err
	}
	if err := validatePurchasable(event); err != nil {
		return nil, err
	}
	unitPrice := *event.TicketPriceMinor

	// Idempotency replay: a retried purchase resolves to its own ticket. The
	// ownership guard inside replay stops a second buyer using the same
	// (public event id, guessable key) from receiving the first buyer's ticket,
	// PII or payment URL.
	if existing, err := u.tickets.GetByIdempotencyKey(ctx, in.EventID, in.IdempotencyKey); err == nil {
		return u.replay(ctx, actor, in, existing)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	ticket, raced, err := u.reserve(ctx, actor, event, unitPrice, in)
	if err != nil {
		return nil, err
	}
	// A RACED ticket belongs to whoever won the idempotency-insert (possibly a
	// DIFFERENT buyer — the event id is public and the key is client-chosen). It
	// MUST go through the ownership-guarded replay, which rejects a non-owner
	// with ErrForbidden before revealing anything, and only repairs / returns the
	// ticket to its rightful buyer. Falling through to createTicketPayment /
	// release here would leak the winner's ticket + PII and let an attacker
	// cancel a paying victim's in-flight ticket — never touch a ticket this
	// request did not create.
	if raced {
		return u.replay(ctx, actor, in, ticket)
	}

	payment, err := u.createTicketPayment(ctx, actor, in, ticket)
	if err != nil {
		// Payment could not be started for OUR OWN freshly-reserved ticket —
		// release the held seat so it is not stranded pending forever.
		// Best-effort: a release failure is logged by the CAS layer; the
		// pending-ticket sweep is the backstop.
		_ = u.release(ctx, ticket)
		return nil, err
	}
	return &PurchaseResult{Ticket: ticket, Payment: payment}, nil
}

// createTicketPayment creates the payment intent for a reserved ticket and
// links it. The idempotency key is the ticket's own (server-scoped to the
// ticket), so a re-attempt — including the repair path in replay — resolves to
// the same acquirer hold instead of charging twice.
func (u *purchaseUseCase) createTicketPayment(ctx context.Context, actor Actor, in PurchaseInput, ticket *domain.EventTicket) (*domain.Payment, error) {
	payment, err := u.payments.CreateForTicket(ctx, actor.payment(), payments.TicketPaymentInput{
		EventTicketID:   ticket.ID,
		RestaurantID:    ticket.RestaurantID,
		UserID:          ticket.UserID,
		BaseAmountMinor: ticket.TotalMinor,
		Currency:        ticket.Currency,
		IdempotencyKey:  ticket.PurchaseIdempotencyKey,
		ReturnURL:       in.ReturnURL,
		CallbackURL:     in.CallbackURL,
		CustomerPhone:   ticket.GuestPhone,
		CustomerEmail:   ticket.GuestEmail,
	})
	if err != nil {
		return nil, err
	}
	if serr := u.tickets.SetPaymentID(ctx, ticket.ID, payment.ID); serr != nil {
		return nil, serr
	}
	ticket.PaymentID = &payment.ID
	return payment, nil
}

// reserve holds the seats: lock the event, enforce capacity, insert the pending
// ticket — all in one transaction so two concurrent buyers cannot oversell. The
// returned bool is true when this request LOST the idempotency-insert race and
// the returned ticket is the winner's (possibly a foreign buyer's) — the caller
// must route that through the ownership-guarded replay, never act on it directly.
func (u *purchaseUseCase) reserve(ctx context.Context, actor Actor, event *domain.Event, unitPrice int64, in PurchaseInput) (*domain.EventTicket, bool, error) {
	total := unitPrice * int64(in.Quantity)
	ticket := &domain.EventTicket{
		ID:                     uuid.New(),
		EventID:                event.ID,
		RestaurantID:           event.RestaurantID,
		UserID:                 actor.UserID,
		GuestName:              in.GuestName,
		GuestPhone:             in.GuestPhone,
		GuestEmail:             in.GuestEmail,
		Quantity:               in.Quantity,
		UnitPriceMinor:         unitPrice,
		TotalMinor:             total,
		Currency:               domain.CurrencyKZT,
		Status:                 domain.TicketPending,
		PurchaseIdempotencyKey: in.IdempotencyKey,
		// Freeze the venue's refund rules onto the ticket, exactly like the
		// price above: the terms promise the guest that a later change by the
		// venue does not apply to a ticket already bought, so the refund path
		// reads this snapshot and never the event's current columns.
		RefundPolicyRefundable:    event.TicketsRefundable,
		RefundPolicyCutoffMinutes: event.TicketRefundCutoffMinutes,
	}

	var raced *domain.EventTicket
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.tickets.LockEventForCapacity(ctx, event.ID); err != nil {
			return err
		}
		if event.Capacity != nil {
			sold, err := u.tickets.SoldCount(ctx, event.ID)
			if err != nil {
				return err
			}
			if sold+in.Quantity > *event.Capacity {
				remaining := *event.Capacity - sold
				if remaining < 0 {
					remaining = 0
				}
				return fmt.Errorf("%w: only %d of %d tickets remain for this event",
					domain.ErrAlreadyExists, remaining, *event.Capacity)
			}
		}
		if err := u.tickets.Create(ctx, ticket); err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				// Lost the idempotency-key race to a concurrent identical retry.
				got, gerr := u.tickets.GetByIdempotencyKey(ctx, event.ID, in.IdempotencyKey)
				if gerr != nil {
					return gerr
				}
				raced = got
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if raced != nil {
		return raced, true, nil
	}
	return ticket, false, nil
}

// replay returns the existing ticket for a retried purchase, but only to its
// rightful buyer (authorizeReplay) — an event id is public and idempotency keys
// are client-chosen with no entropy, so without this guard a second buyer
// posting the same (event, key) would receive the first buyer's ticket, PII and
// payment URL. It also REPAIRS a pending ticket that was left without a payment
// (SetPaymentID failed, or a crash between reserve and payment creation): it
// re-creates the payment idempotently so the guest can actually pay, instead of
// holding the seat forever with no way to complete checkout.
func (u *purchaseUseCase) replay(ctx context.Context, actor Actor, in PurchaseInput, ticket *domain.EventTicket) (*PurchaseResult, error) {
	if err := authorizeReplay(actor, in, ticket); err != nil {
		return nil, err
	}
	if ticket.Status == domain.TicketPending && ticket.PaymentID == nil {
		payment, err := u.createTicketPayment(ctx, actor, in, ticket)
		if err != nil {
			return nil, err
		}
		return &PurchaseResult{Ticket: ticket, Payment: payment}, nil
	}
	res := &PurchaseResult{Ticket: ticket}
	if ticket.PaymentID != nil {
		p, err := u.paymentByID(ctx, *ticket.PaymentID)
		if err != nil {
			return nil, err
		}
		res.Payment = p
	}
	return res, nil
}

// authorizeReplay ensures a retried purchase resolves to its OWN buyer's
// ticket. An account buyer must be the same authenticated user; an anonymous
// buyer must present the same normalised guest phone the ticket was bought
// with. A mismatch is ErrForbidden — the caller never sees the other buyer's
// ticket or PII. A superadmin/staff actor is allowed (admin tooling), but a
// plain venue-staff role is treated like any other non-owner unless it is the
// buyer's account.
func authorizeReplay(actor Actor, in PurchaseInput, existing *domain.EventTicket) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	if existing.UserID != nil {
		if actor.UserID == nil || *actor.UserID != *existing.UserID {
			return fmt.Errorf("%w: this idempotency key belongs to another buyer", domain.ErrForbidden)
		}
		return nil
	}
	// Anonymous purchase: the phone is the only ownership proof available.
	if in.GuestPhone == "" || in.GuestPhone != existing.GuestPhone {
		return fmt.Errorf("%w: this idempotency key belongs to another buyer", domain.ErrForbidden)
	}
	return nil
}

// release moves a pending reservation to cancelled, freeing its held capacity.
func (u *purchaseUseCase) release(ctx context.Context, ticket *domain.EventTicket) error {
	return u.tickets.CompareAndSwapStatus(ctx, ticket.ID, domain.TicketPending, domain.TicketCancelled, time.Now())
}

// validatePurchasable rejects a purchase for an event that is not ticketed, not
// published, has no price, or has already ended.
func validatePurchasable(e *domain.Event) error {
	if !e.Ticketed || e.TicketPriceMinor == nil {
		return fmt.Errorf("%w: this event does not sell tickets", domain.ErrValidation)
	}
	if e.Status != domain.EventPublished {
		return fmt.Errorf("%w: tickets are only on sale for a published event", domain.ErrValidation)
	}
	if !e.EndsAt.After(time.Now()) {
		return fmt.Errorf("%w: this event has already ended", domain.ErrValidation)
	}
	if *e.TicketPriceMinor <= 0 {
		return fmt.Errorf("%w: this event's ticket price is not set", domain.ErrValidation)
	}
	return nil
}
