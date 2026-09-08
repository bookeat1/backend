// Package tickets is the application logic for event-ticket purchasing: it owns
// event validation, race-safe capacity enforcement (no overselling) and the
// ticket lifecycle, and delegates every money movement to usecase/payments via
// the ticketPayments port. Layering, same convention as usecase/bookings and
// usecase/payments:
//   - this package never imports internal/infrastructure/*; the capabilities it
//     needs are minimal local ports bound to concrete implementations in
//     bootstrap/deps.go;
//   - all money (gross-up, acquirer calls, ledger, refund CAS) lives in
//     usecase/payments (the ticketPayments port) — this package only decides
//     WHO may buy/refund and holds/frees CAPACITY, never touches an amount.
package tickets

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/payments"
)

// Actor is the authenticated caller. UserID is nil for a guest checkout without
// an account (the transport layer only reaches this package after verifying the
// buyer's contact, same as bookings/payments). Role drives staff/admin checks.
type Actor struct {
	UserID *uuid.UUID
	Role   domain.Role
}

func (a Actor) staff() bool { return a.Role == domain.RoleRestaurant || a.Role == domain.RoleAdmin }

// payment converts to the payments-package actor shape.
func (a Actor) payment() payments.Actor { return payments.Actor{UserID: a.UserID, Role: a.Role} }

// eventReader is the minimal slice of domain.EventRepository this package needs:
// read an event to validate it is ticketed/published and read its price/capacity.
type eventReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Event, error)
}

// ticketPayments is the money half of a purchase/refund, implemented by
// usecase/payments.TicketPaymentUseCase. It creates the payment intent (hold,
// grossed up, captured-on-authorize like a pre-order) and refunds a captured
// ticket payment. This package never calls an acquirer or touches the ledger
// itself — that all lives behind this port.
type ticketPayments interface {
	CreateForTicket(ctx context.Context, actor payments.Actor, in payments.TicketPaymentInput) (*domain.Payment, error)
	RefundTicket(ctx context.Context, actor payments.Actor, in payments.TicketRefundInput) (*domain.Payment, error)
}

// paymentReader is the minimal slice of domain.PaymentRepository this package
// needs: read a ticket's payment to return it on a purchase replay / status
// read. It never mutates a payment — every payment write goes through
// ticketPayments (usecase/payments).
type paymentReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
}

// permissionChecker answers "may this user perform perm at this restaurant" per
// the domain RBAC matrix (bound to restaurants.ManagerUseCase). Unaware of the
// global superadmin — this package checks RoleAdmin FIRST and bypasses it, the
// same contract every other HasPermission call site keeps.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}
