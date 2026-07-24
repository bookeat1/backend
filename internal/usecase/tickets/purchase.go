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

	event, err := u.events.GetByID(ctx, in.EventID)
	if err != nil {
		return nil, err
	}
	if err := validatePurchasable(event); err != nil {
		return nil, err
	}
	unitPrice := *event.TicketPriceMinor

	// Idempotency replay: a retried purchase resolves to its own ticket.
	if existing, err := u.tickets.GetByIdempotencyKey(ctx, in.EventID, in.IdempotencyKey); err == nil {
		return u.replay(ctx, existing)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	ticket, err := u.reserve(ctx, actor, event, unitPrice, in)
	if err != nil {
		return nil, err
	}
	// A reservation-race replay (the idempotency insert lost) already carries its
	// payment; return it as-is.
	if ticket.Status != domain.TicketPending || ticket.PaymentID != nil {
		return u.replay(ctx, ticket)
	}

	payment, err := u.payments.CreateForTicket(ctx, actor.payment(), payments.TicketPaymentInput{
		EventTicketID:   ticket.ID,
		RestaurantID:    ticket.RestaurantID,
		UserID:          ticket.UserID,
		BaseAmountMinor: ticket.TotalMinor,
		Currency:        ticket.Currency,
		IdempotencyKey:  in.IdempotencyKey,
		ReturnURL:       in.ReturnURL,
		CallbackURL:     in.CallbackURL,
		CustomerPhone:   ticket.GuestPhone,
		CustomerEmail:   ticket.GuestEmail,
	})
	if err != nil {
		// Payment could not be started — release the held seat so it is not
		// stranded pending forever. Best-effort: a release failure is logged by
		// the CAS layer; the hold TTL / reconciler is the backstop.
		_ = u.release(ctx, ticket)
		return nil, err
	}

	if serr := u.tickets.SetPaymentID(ctx, ticket.ID, payment.ID); serr != nil {
		return nil, serr
	}
	ticket.PaymentID = &payment.ID
	return &PurchaseResult{Ticket: ticket, Payment: payment}, nil
}

// reserve holds the seats: lock the event, enforce capacity, insert the pending
// ticket — all in one transaction so two concurrent buyers cannot oversell.
func (u *purchaseUseCase) reserve(ctx context.Context, actor Actor, event *domain.Event, unitPrice int64, in PurchaseInput) (*domain.EventTicket, error) {
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
		return nil, err
	}
	if raced != nil {
		return raced, nil
	}
	return ticket, nil
}

// replay returns the existing ticket with its payment loaded (if any). A
// cancelled reservation is returned as-is (no payment); the client must use a
// fresh idempotency key to try again.
func (u *purchaseUseCase) replay(ctx context.Context, ticket *domain.EventTicket) (*PurchaseResult, error) {
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
