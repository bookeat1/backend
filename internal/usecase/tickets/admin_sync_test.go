package tickets

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TestAdminListSoldRBAC: a staff caller without PermRestaurantManage at the
// event's restaurant is forbidden; a superadmin and a permitted manager succeed.
func TestAdminListSoldRBAC(t *testing.T) {
	repo := newFakeTicketRepo()
	event := ticketedEvent(ptr(100), 35000)
	events := newFakeEvents(event)

	// Denied: staff without permission.
	denied := NewAdminUseCase(repo, events, &fakePerms{allow: false})
	if _, _, err := denied.ListSold(context.Background(), Actor{UserID: ptr(uuid.New()), Role: domain.RoleRestaurant}, event.ID, nil, 1, 20); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("denied staff: err = %v, want ErrForbidden", err)
	}

	// Allowed: manager with permission.
	allowed := NewAdminUseCase(repo, events, &fakePerms{allow: true})
	if _, _, err := allowed.ListSold(context.Background(), Actor{UserID: ptr(uuid.New()), Role: domain.RoleRestaurant}, event.ID, nil, 1, 20); err != nil {
		t.Fatalf("permitted manager: %v", err)
	}

	// Allowed: superadmin bypasses the permission check entirely.
	super := NewAdminUseCase(repo, events, &fakePerms{allow: false})
	if _, err := super.Counts(context.Background(), Actor{UserID: ptr(uuid.New()), Role: domain.RoleAdmin}, event.ID); err != nil {
		t.Fatalf("superadmin: %v", err)
	}
}

// TestPaymentObserverProjection: the observer maps payment status onto ticket
// status (payment success → paid; failure → cancelled; refund → refunded), and
// is idempotent on a redelivered event.
func TestPaymentObserverProjection(t *testing.T) {
	newTicket := func(status domain.TicketStatus) (*fakeTicketRepo, uuid.UUID) {
		repo := newFakeTicketRepo()
		id := uuid.New()
		repo.byID[id] = &domain.EventTicket{ID: id, EventID: uuid.New(), Status: status, Quantity: 1}
		return repo, id
	}

	cases := []struct {
		name    string
		payment domain.PaymentStatus
		from    domain.TicketStatus
		want    domain.TicketStatus
	}{
		{"captured→paid", domain.PaymentCaptured, domain.TicketPending, domain.TicketPaid},
		{"failed→cancelled", domain.PaymentFailed, domain.TicketPending, domain.TicketCancelled},
		{"expired→cancelled", domain.PaymentExpired, domain.TicketPending, domain.TicketCancelled},
		{"voided→cancelled", domain.PaymentVoided, domain.TicketPending, domain.TicketCancelled},
		{"refunded→refunded", domain.PaymentRefunded, domain.TicketPaid, domain.TicketRefunded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, ticketID := newTicket(tc.from)
			obs := NewPaymentObserver(repo)
			p := &domain.Payment{ID: uuid.New(), EventTicketID: &ticketID, Status: tc.payment}
			if err := obs.OnPaymentApplied(context.Background(), p); err != nil {
				t.Fatalf("OnPaymentApplied: %v", err)
			}
			if got := repo.byID[ticketID].Status; got != tc.want {
				t.Fatalf("ticket status = %s, want %s", got, tc.want)
			}
			// Idempotent redelivery: applying again is a no-op, not an error.
			if err := obs.OnPaymentApplied(context.Background(), p); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if got := repo.byID[ticketID].Status; got != tc.want {
				t.Fatalf("redelivery changed status to %s", got)
			}
		})
	}
}

// TestPaymentObserverIgnoresBookingPayments: a payment with no EventTicketID is
// never projected (booking payments are untouched).
func TestPaymentObserverIgnoresBookingPayments(t *testing.T) {
	repo := newFakeTicketRepo()
	obs := NewPaymentObserver(repo)
	if err := obs.OnPaymentApplied(context.Background(), &domain.Payment{ID: uuid.New(), Status: domain.PaymentCaptured}); err != nil {
		t.Fatalf("booking payment: %v", err)
	}
}
