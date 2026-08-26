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

// signedIn is the buyer identity every purchase now needs: a ticket is sold to
// an ACCOUNT, never to an anonymous caller (owner decision 2026-07-25).
func signedIn(id uuid.UUID) Actor { return Actor{UserID: &id, Role: domain.RoleUser} }

// TestPurchaseHappyPath: a valid purchase reserves the seats (pending ticket),
// creates the payment for quantity × price, and links it.
func TestPurchaseHappyPath(t *testing.T) {
	event := ticketedEvent(ptr(10), 35000)
	uc, repo, pay := newPurchaseHarness(t, event)
	buyer := uuid.New()

	res, err := uc.Purchase(context.Background(), signedIn(buyer), PurchaseInput{
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

	if _, err := uc.Purchase(context.Background(), signedIn(uuid.New()), PurchaseInput{
		EventID: event.ID, Quantity: 2, GuestPhone: "+7700", IdempotencyKey: "a", CallbackURL: "https://cb",
	}); err != nil {
		t.Fatalf("first purchase: %v", err)
	}
	// 2 sold, capacity 3 → asking for 2 more must be rejected (only 1 remains).
	_, err := uc.Purchase(context.Background(), signedIn(uuid.New()), PurchaseInput{
		EventID: event.ID, Quantity: 2, GuestPhone: "+7700", IdempotencyKey: "b", CallbackURL: "https://cb",
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

	_, err := uc.Purchase(context.Background(), signedIn(uuid.New()), PurchaseInput{
		EventID: event.ID, Quantity: 2, GuestPhone: "+7700", IdempotencyKey: "k", CallbackURL: "https://cb",
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
	buyer := uuid.New()

	r1, err := uc.Purchase(context.Background(), signedIn(buyer), PurchaseInput{
		EventID: event.ID, Quantity: 1, GuestPhone: "+7700", IdempotencyKey: "same", CallbackURL: "https://cb",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := uc.Purchase(context.Background(), signedIn(buyer), PurchaseInput{
		EventID: event.ID, Quantity: 1, GuestPhone: "+7700", IdempotencyKey: "same", CallbackURL: "https://cb",
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

// TestReplayRepairsMissingPayment: a retry of a pending ticket left with NO
// payment (SetPaymentID failed / crash before payment creation) re-creates the
// payment so the guest can actually pay, instead of holding the seat with no
// checkout path.
func TestReplayRepairsMissingPayment(t *testing.T) {
	event := ticketedEvent(ptr(10), 35000)
	uc, repo, pay := newPurchaseHarness(t, event)
	buyer := uuid.New()
	// A pending ticket that never got a payment linked.
	tk := &domain.EventTicket{
		ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
		UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
		Status: domain.TicketPending, UserID: &buyer, PurchaseIdempotencyKey: "k", GuestPhone: "+7700",
	}
	repo.byID[tk.ID] = tk

	res, err := uc.Purchase(context.Background(), signedIn(buyer), PurchaseInput{
		EventID: event.ID, Quantity: 1, GuestPhone: "+7700", IdempotencyKey: "k", CallbackURL: "https://cb",
	})
	if err != nil {
		t.Fatalf("Purchase (repair): %v", err)
	}
	if res.Payment == nil {
		t.Fatalf("repair did not create a payment")
	}
	if pay.createN != 1 {
		t.Fatalf("CreateForTicket called %d times, want 1", pay.createN)
	}
	if repo.byID[tk.ID].PaymentID == nil {
		t.Fatalf("payment id not linked after repair")
	}
}

// TestReplayRejectsForeignBuyer: a second buyer posting the same (public event,
// guessable key) must NOT receive the first buyer's ticket, PII or payment URL.
func TestReplayRejectsForeignBuyer(t *testing.T) {
	event := ticketedEvent(ptr(10), 35000)

	// Account buyer A owns the ticket; attacker B (different user) is rejected.
	t.Run("account buyer", func(t *testing.T) {
		uc, repo, _ := newPurchaseHarness(t, event)
		userA := uuid.New()
		tk := &domain.EventTicket{
			ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
			UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
			Status: domain.TicketPaid, UserID: &userA, PurchaseIdempotencyKey: "1", GuestName: "Alice",
		}
		repo.byID[tk.ID] = tk
		_, err := uc.Purchase(context.Background(), Actor{UserID: ptr(uuid.New()), Role: domain.RoleUser}, PurchaseInput{
			EventID: event.ID, Quantity: 1, IdempotencyKey: "1", CallbackURL: "https://cb",
		})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("foreign account buyer err = %v, want ErrForbidden", err)
		}
	})

	// A legacy ticket sold before accounts were required still cannot be reached
	// by a typed-in phone: the anonymous path is gone entirely, so the caller is
	// refused before ownership is even considered.
	t.Run("anonymous caller is refused outright", func(t *testing.T) {
		uc, repo, _ := newPurchaseHarness(t, event)
		tk := &domain.EventTicket{
			ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
			UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
			Status: domain.TicketPaid, PurchaseIdempotencyKey: "1", GuestPhone: "+7700111", GuestName: "Alice",
		}
		repo.byID[tk.ID] = tk
		// Knowing the buyer's own phone buys nothing any more.
		for _, phone := range []string{"+7999999", "+7700111"} {
			if _, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
				EventID: event.ID, Quantity: 1, GuestPhone: phone, IdempotencyKey: "1", CallbackURL: "https://cb",
			}); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatalf("anonymous purchase with phone %s: err = %v, want ErrUnauthorized", phone, err)
			}
		}
	})

	// An account buyer replaying their OWN key still gets their own ticket back.
	t.Run("owner replay", func(t *testing.T) {
		uc, repo, _ := newPurchaseHarness(t, event)
		owner := uuid.New()
		tk := &domain.EventTicket{
			ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
			UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
			Status: domain.TicketPaid, UserID: &owner, PurchaseIdempotencyKey: "1", GuestName: "Alice",
		}
		repo.byID[tk.ID] = tk
		res, err := uc.Purchase(context.Background(), signedIn(owner), PurchaseInput{
			EventID: event.ID, Quantity: 1, IdempotencyKey: "1", CallbackURL: "https://cb",
		})
		if err != nil {
			t.Fatalf("owner replay: %v", err)
		}
		if res.Ticket.ID != tk.ID {
			t.Fatalf("owner replay returned a different ticket")
		}
	})
}

// TestReserveRaceRejectsForeignBuyer: the TOCTOU reserve-race path (top-level
// idempotency lookup misses, the insert inside reserve collides with a
// concurrently-created ticket) must ALSO run authorizeReplay — an attacker who
// loses the race to a victim's pending, nil-PaymentID ticket gets ErrForbidden
// and NOTHING of the victim's, and the victim's ticket is left untouched (never
// cancelled by the attacker's failed request).
func TestReserveRaceRejectsForeignBuyer(t *testing.T) {
	t.Run("account victim not cancelled", func(t *testing.T) {
		event := ticketedEvent(ptr(10), 35000)
		uc, repo, pay := newPurchaseHarness(t, event)
		victim := uuid.New()
		vTicket := &domain.EventTicket{
			ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
			UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
			Status: domain.TicketPending, UserID: &victim, PurchaseIdempotencyKey: "1",
			GuestName: "Alice", GuestPhone: "+7700victim",
		}
		repo.byID[vTicket.ID] = vTicket
		repo.suppressLookups = 1 // top lookup misses → reserve insert collides

		attacker := uuid.New()
		_, err := uc.Purchase(context.Background(), Actor{UserID: &attacker, Role: domain.RoleUser}, PurchaseInput{
			EventID: event.ID, Quantity: 1, IdempotencyKey: "1", CallbackURL: "https://cb",
		})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("reserve-race foreign buyer err = %v, want ErrForbidden", err)
		}
		// Victim's ticket must be untouched: still pending, no payment created.
		if repo.byID[vTicket.ID].Status != domain.TicketPending {
			t.Fatalf("victim ticket was cancelled by attacker: %s", repo.byID[vTicket.ID].Status)
		}
		if pay.createN != 0 {
			t.Fatalf("attacker triggered a payment on the victim's ticket: createN=%d", pay.createN)
		}
	})

	// The same race, driven by an anonymous caller against a legacy account-less
	// ticket: refused before any of the victim's data is touched.
	t.Run("anonymous caller cannot race a legacy ticket", func(t *testing.T) {
		event := ticketedEvent(ptr(10), 35000)
		uc, repo, pay := newPurchaseHarness(t, event)
		vTicket := &domain.EventTicket{
			ID: uuid.New(), EventID: event.ID, RestaurantID: *event.RestaurantID, Quantity: 1,
			UnitPriceMinor: 35000, TotalMinor: 35000, Currency: domain.CurrencyKZT,
			Status: domain.TicketPending, PurchaseIdempotencyKey: "1",
			GuestName: "Alice", GuestPhone: "+7700victim", GuestEmail: "alice@x.io",
		}
		repo.byID[vTicket.ID] = vTicket
		repo.suppressLookups = 1

		_, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
			EventID: event.ID, Quantity: 1, GuestPhone: "+7999attacker", IdempotencyKey: "1", CallbackURL: "https://cb",
		})
		if !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("anonymous reserve-race err = %v, want ErrUnauthorized", err)
		}
		if repo.byID[vTicket.ID].Status != domain.TicketPending || pay.createN != 0 {
			t.Fatalf("victim leaked/cancelled: status=%s createN=%d", repo.byID[vTicket.ID].Status, pay.createN)
		}
	})
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
			_, err := uc.Purchase(context.Background(), signedIn(uuid.New()), PurchaseInput{
				EventID: ev.ID, Quantity: 1, GuestPhone: "+7700", IdempotencyKey: "k", CallbackURL: "https://cb",
			})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%s: err = %v, want ErrValidation", name, err)
			}
		})
	}
}
