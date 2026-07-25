package tickets

import (
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/tickets"
)

// ticketResponse is the guest/admin view of a ticket. Money is minor units.
type ticketResponse struct {
	ID             uuid.UUID  `json:"id"`
	EventID        uuid.UUID  `json:"event_id"`
	RestaurantID   uuid.UUID  `json:"restaurant_id"`
	Quantity       int        `json:"quantity"`
	UnitPriceMinor int64      `json:"unit_price_minor"`
	TotalMinor     int64      `json:"total_minor"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	GuestName      string     `json:"guest_name,omitempty"`
	GuestPhone     string     `json:"guest_phone,omitempty"`
	GuestEmail     string     `json:"guest_email,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	// The refund rules this ticket was SOLD under (the purchase-time snapshot,
	// NOT the event's current settings). Always present: a "false" is a rule the
	// app must be able to show. The concrete deadline is starts_at minus the
	// cutoff — the app computes it from the event it already renders, so this
	// shape needs no join.
	RefundPolicyRefundable    bool      `json:"refund_policy_refundable"`
	RefundPolicyCutoffMinutes int       `json:"refund_policy_cutoff_minutes"`
	CreatedAt                 time.Time `json:"created_at"`
}

func ticketToResponse(t domain.EventTicket) ticketResponse {
	return ticketResponse{
		ID: t.ID, EventID: t.EventID, RestaurantID: t.RestaurantID,
		Quantity: t.Quantity, UnitPriceMinor: t.UnitPriceMinor, TotalMinor: t.TotalMinor,
		Currency: string(t.Currency), Status: string(t.Status),
		GuestName: t.GuestName, GuestPhone: t.GuestPhone, GuestEmail: t.GuestEmail,
		PaymentID: t.PaymentID, CreatedAt: t.CreatedAt,
		RefundPolicyRefundable:    t.RefundPolicyRefundable,
		RefundPolicyCutoffMinutes: t.RefundPolicyCutoffMinutes,
	}
}

// purchaseResponse is the buy result: the ticket plus, when the payment needs a
// hosted card page, the URL to send the guest to. The ticket is `pending` until
// the acquirer confirms capture (webhook), at which point it becomes `paid`.
type purchaseResponse struct {
	Ticket     ticketResponse `json:"ticket"`
	PaymentID  *uuid.UUID     `json:"payment_id,omitempty"`
	PaymentURL *string        `json:"payment_url,omitempty"`
}

func purchaseToResponse(r *uc.PurchaseResult) purchaseResponse {
	out := purchaseResponse{Ticket: ticketToResponse(*r.Ticket)}
	if r.Payment != nil {
		id := r.Payment.ID
		out.PaymentID = &id
		out.PaymentURL = r.Payment.PaymentURL
	}
	return out
}

// countsResponse is the admin tickets-sold aggregate for an event.
type countsResponse struct {
	EventID          uuid.UUID `json:"event_id"`
	PendingTickets   int       `json:"pending_tickets"`
	PendingQuantity  int       `json:"pending_quantity"`
	PaidTickets      int       `json:"paid_tickets"`
	PaidQuantity     int       `json:"paid_quantity"`
	RefundedTickets  int       `json:"refunded_tickets"`
	CancelledTickets int       `json:"cancelled_tickets"`
	RevenuePaidMinor int64     `json:"revenue_paid_minor"`
	Currency         string    `json:"currency"`
}

func countsToResponse(c domain.EventTicketCounts) countsResponse {
	return countsResponse{
		EventID: c.EventID, PendingTickets: c.PendingTickets, PendingQuantity: c.PendingQuantity,
		PaidTickets: c.PaidTickets, PaidQuantity: c.PaidQuantity, RefundedTickets: c.RefundedTickets,
		CancelledTickets: c.CancelledTickets, RevenuePaidMinor: c.RevenuePaidMinor, Currency: string(c.Currency),
	}
}
