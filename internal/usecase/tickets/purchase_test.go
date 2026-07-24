package tickets

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newPurchaseHarness(t *testing.T, event *domain.Event) (*purchaseUseCase, *fakeTicketRepo, *fakeTicketPayments) {
	t.Helper()
	repo := newFakeTicketRepo()
	pay := &fakeTicketPayments{}
	events := newFakeEvents(event)
	uc := NewPurchaseUseCase(repo, events, pay, &fakePaymentReader{}, fakeTx{}).(*purchaseUseCase)
	return uc, repo, pay
}

// fakePaymentReader returns a synthetic payment for any id (the payment always
// exists once a ticket links it) — enough for replay to load one.
type fakePaymentReader struct{}

func (f *fakePaymentReader) GetByID(_ context.Context, id uuid.UUID) (*domain.Payment, error) {
	return &domain.Payment{ID: id, Purpose: domain.PurposeTicket, Status: domain.PaymentCreated}, nil
}

// TestPurchaseHappyPath: a valid purchase reserves the seats (pending ticket),
// creates the payment for quantity × price, and links it.
func TestPurchaseHappyPath(t *testing.T) {
	event := ticketedEvent(ptr(10), 35000)
	uc, repo, pay := newPurchaseHarness(t, event)

	res, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 2, GuestPhone: "+7700", IdempotencyKey: "k1", CallbackURL: "https://cb",
	})
	if err != nil {
		t.Fatalf("Purchase: %v", err)
	}
	if res.Ticket.Status != domain.TicketPending {
		t.Fatalf("ticket status = %s, want pending", res.Ticket.Status)
	}
	if res.Ticket.TotalMinor != 70000 || res.Ticket.UnitPriceMinor != 35000 {
		t.Fatalf("pricing wrong: unit=%d total=%d", res.Ticket.UnitPriceMinor, res.Ticket.TotalMinor)
	}
	if pay.lastCreate.BaseAmountMinor != 70000 {
		t.Fatalf("payment base = %d, want 70000 (2×35000)", pay.lastCreate.BaseAmountMinor)
	}
	if res.Ticket.PaymentID == nil {
		t.Fatalf("payment id not linked onto ticket")
	}
	if sold, _ := repo.SoldCount(context.Background(), event.ID); sold != 2 {
		t.Fatalf("sold = %d, want 2", sold)
	}
}

// TestPurchaseCapacityEnforced: cannot buy more than the remaining capacity.
func TestPurchaseCapacityEnforced(t *testing.T) {
	event := ticketedEvent(ptr(3), 35000)
	uc, _, _ := newPurchaseHarness(t, event)

	if _, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 2, IdempotencyKey: "a", CallbackURL: "https://cb",
	}); err != nil {
		t.Fatalf("first purchase: %v", err)
	}
	// 2 sold, capacity 3 → asking for 2 more must be rejected (only 1 remains).
	_, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 2, IdempotencyKey: "b", CallbackURL: "https://cb",
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("oversell not rejected: err = %v", err)
	}
}

// TestPurchasePaymentFailureReleasesCapacity: when the payment cannot be
// started, the reserved seat is released so it is not stranded.
func TestPurchasePaymentFailureReleasesCapacity(t *testing.T) {
	event := ticketedEvent(ptr(5), 35000)
	uc, repo, pay := newPurchaseHarness(t, event)
	pay.createErr = errors.New("acquirer down")

	_, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 2, IdempotencyKey: "k", CallbackURL: "https://cb",
	})
	if err == nil {
		t.Fatalf("expected purchase to fail")
	}
	// The pending reservation must have been released → capacity freed.
	if sold, _ := repo.SoldCount(context.Background(), event.ID); sold != 0 {
		t.Fatalf("capacity not released: sold = %d, want 0", sold)
	}
}

// TestPurchaseIdempotentReplay: a retry with the same key returns the same
// ticket and never creates a second payment.
func TestPurchaseIdempotentReplay(t *testing.T) {
	event := ticketedEvent(ptr(10), 35000)
	uc, _, pay := newPurchaseHarness(t, event)

	r1, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 1, IdempotencyKey: "same", CallbackURL: "https://cb",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 1, IdempotencyKey: "same", CallbackURL: "https://cb",
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r1.Ticket.ID != r2.Ticket.ID {
		t.Fatalf("retry made a new ticket: %s vs %s", r1.Ticket.ID, r2.Ticket.ID)
	}
	if pay.createN != 1 {
		t.Fatalf("payment created %d times on a retry, want 1", pay.createN)
	}
}

// TestPurchaseRejectsNonTicketedAndUnpublished.
func TestPurchaseRejectsNonTicketedAndUnpublished(t *testing.T) {
	price := int64(35000)
	cases := map[string]*domain.Event{
		"not ticketed": {ID: uuid.New(), Ticketed: false, Status: domain.EventPublished, EndsAt: nowPlus()},
		"unpublished":  {ID: uuid.New(), Ticketed: true, TicketPriceMinor: &price, Status: domain.EventDraft, EndsAt: nowPlus()},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			uc, _, _ := newPurchaseHarness(t, ev)
			_, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
				EventID: ev.ID, Quantity: 1, IdempotencyKey: "k", CallbackURL: "https://cb",
			})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%s: err = %v, want ErrValidation", name, err)
			}
		})
	}
}
