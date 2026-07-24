package tickets

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TestRefundReturnsCapacity: refunding a PAID ticket moves it to refunded,
// which frees its held capacity (SoldCount drops).
func TestRefundReturnsCapacity(t *testing.T) {
	repo := newFakeTicketRepo()
	pay := &fakeTicketPayments{}
	eventID := uuid.New()
	pid := uuid.New()
	paid := &domain.EventTicket{
		ID: uuid.New(), EventID: eventID, RestaurantID: uuid.New(), Quantity: 2,
		UnitPriceMinor: 35000, TotalMinor: 70000, Currency: domain.CurrencyKZT,
		Status: domain.TicketPaid, PaymentID: &pid,
	}
	repo.byID[paid.ID] = paid

	if sold, _ := repo.SoldCount(context.Background(), eventID); sold != 2 {
		t.Fatalf("precondition: sold = %d, want 2", sold)
	}

	uc := NewRefundUseCase(repo, pay)
	tk, err := uc.Refund(context.Background(), Actor{}, paid.ID, RefundInput{IdempotencyKey: "r1"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if tk.Status != domain.TicketRefunded {
		t.Fatalf("ticket status = %s, want refunded", tk.Status)
	}
	if pay.refundN != 1 {
		t.Fatalf("payment refund called %d times, want 1", pay.refundN)
	}
	if sold, _ := repo.SoldCount(context.Background(), eventID); sold != 0 {
		t.Fatalf("capacity not returned: sold = %d, want 0", sold)
	}
}

// TestRefundRejectsUnpaidTicket: a pending (not-yet-paid) ticket cannot be
// refunded.
func TestRefundRejectsUnpaidTicket(t *testing.T) {
	repo := newFakeTicketRepo()
	pending := &domain.EventTicket{ID: uuid.New(), EventID: uuid.New(), Status: domain.TicketPending, Quantity: 1}
	repo.byID[pending.ID] = pending
	uc := NewRefundUseCase(repo, &fakeTicketPayments{})
	_, err := uc.Refund(context.Background(), Actor{}, pending.ID, RefundInput{IdempotencyKey: "r"})
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus", err)
	}
}

// TestRefundIdempotent: an already-refunded ticket returns as-is without a
// second money call.
func TestRefundIdempotent(t *testing.T) {
	repo := newFakeTicketRepo()
	pay := &fakeTicketPayments{}
	refunded := &domain.EventTicket{ID: uuid.New(), EventID: uuid.New(), Status: domain.TicketRefunded, Quantity: 1}
	repo.byID[refunded.ID] = refunded
	uc := NewRefundUseCase(repo, pay)
	tk, err := uc.Refund(context.Background(), Actor{}, refunded.ID, RefundInput{IdempotencyKey: "r"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if tk.Status != domain.TicketRefunded || pay.refundN != 0 {
		t.Fatalf("idempotent refund broke: status=%s refundN=%d", tk.Status, pay.refundN)
	}
}
