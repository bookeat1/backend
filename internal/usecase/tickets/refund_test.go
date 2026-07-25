package tickets

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// refundFixture builds a paid ticket for a published event starting in 24h,
// sold under the given policy snapshot, plus the fakes the refund usecase
// needs. buyer is the account that bought it (nil = an account-less guest
// checkout).
func refundFixture(buyer *uuid.UUID, refundable bool, cutoffMinutes int) (*fakeTicketRepo, *fakeEvents, *domain.EventTicket) {
	repo := newFakeTicketRepo()
	event := ticketedEvent(nil, 35000)
	pid := uuid.New()
	t := &domain.EventTicket{
		ID: uuid.New(), EventID: event.ID, RestaurantID: event.RestaurantID, UserID: buyer,
		Quantity: 2, UnitPriceMinor: 35000, TotalMinor: 70000, Currency: domain.CurrencyKZT,
		Status: domain.TicketPaid, PaymentID: &pid,
		RefundPolicyRefundable:    refundable,
		RefundPolicyCutoffMinutes: cutoffMinutes,
	}
	repo.byID[t.ID] = t
	return repo, newFakeEvents(event), t
}

// TestRefundReturnsCapacity: refunding a PAID ticket moves it to refunded,
// which frees its held capacity (SoldCount drops).
func TestRefundReturnsCapacity(t *testing.T) {
	buyer := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 60)
	pay := &fakeTicketPayments{}

	if sold, _ := repo.SoldCount(context.Background(), paid.EventID); sold != 2 {
		t.Fatalf("precondition: sold = %d, want 2", sold)
	}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	tk, err := uc.Refund(context.Background(), Actor{UserID: &buyer}, paid.ID, RefundInput{IdempotencyKey: "r1"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if tk.Status != domain.TicketRefunded {
		t.Fatalf("ticket status = %s, want refunded", tk.Status)
	}
	if pay.refundN != 1 {
		t.Fatalf("payment refund called %d times, want 1", pay.refundN)
	}
	if sold, _ := repo.SoldCount(context.Background(), paid.EventID); sold != 0 {
		t.Fatalf("capacity not returned: sold = %d, want 0", sold)
	}
}

// TestRefundRejectsUnpaidTicket: a pending (not-yet-paid) ticket cannot be
// refunded.
func TestRefundRejectsUnpaidTicket(t *testing.T) {
	repo := newFakeTicketRepo()
	pending := &domain.EventTicket{ID: uuid.New(), EventID: uuid.New(), Status: domain.TicketPending, Quantity: 1}
	repo.byID[pending.ID] = pending
	uc := NewRefundUseCase(repo, newFakeEvents(), &fakeTicketPayments{}, &fakePerms{})
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
	uc := NewRefundUseCase(repo, newFakeEvents(), pay, &fakePerms{})
	tk, err := uc.Refund(context.Background(), Actor{}, refunded.ID, RefundInput{IdempotencyKey: "r"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if tk.Status != domain.TicketRefunded || pay.refundN != 0 {
		t.Fatalf("idempotent refund broke: status=%s refundN=%d", tk.Status, pay.refundN)
	}
}

// TestRefundGuestAllowedWithinWindow: the buyer may self-refund their own
// ticket while the event's policy (as frozen on the ticket) still allows it —
// no staff involvement at all.
func TestRefundGuestAllowedWithinWindow(t *testing.T) {
	buyer := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 120) // the event starts in 24h
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	tk, err := uc.Refund(context.Background(), Actor{UserID: &buyer}, paid.ID, RefundInput{IdempotencyKey: "g1"})
	if err != nil {
		t.Fatalf("guest self-refund: %v", err)
	}
	if tk.Status != domain.TicketRefunded || pay.refundN != 1 {
		t.Fatalf("status=%s refundN=%d, want refunded/1", tk.Status, pay.refundN)
	}
}

// TestRefundGuestTooLate: the same ticket, but the cutoff (48h) has already
// passed for an event starting in 24h — the guest is refused and, crucially, no
// money call is made.
func TestRefundGuestTooLate(t *testing.T) {
	buyer := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 48*60)
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	_, err := uc.Refund(context.Background(), Actor{UserID: &buyer}, paid.ID, RefundInput{IdempotencyKey: "g2"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved on a refused refund: refundN = %d", pay.refundN)
	}
	if got, _ := repo.GetByID(context.Background(), paid.ID); got.Status != domain.TicketPaid {
		t.Fatalf("ticket status = %s, want it left paid", got.Status)
	}
}

// TestRefundGuestNonRefundableEvent: a venue that sells this event as final
// sale refuses the guest regardless of how early they ask.
func TestRefundGuestNonRefundableEvent(t *testing.T) {
	buyer := uuid.New()
	repo, events, paid := refundFixture(&buyer, false, 60)
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	_, err := uc.Refund(context.Background(), Actor{UserID: &buyer}, paid.ID, RefundInput{IdempotencyKey: "g3"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved on a non-refundable ticket: refundN = %d", pay.refundN)
	}
}

// TestRefundStaffOverride: venue staff holding payment.refund may refund a
// ticket the policy refuses — but ONLY with the explicit override. The same
// call without it is rejected, which is what makes the exception a decision
// rather than an accident.
func TestRefundStaffOverride(t *testing.T) {
	buyer := uuid.New()
	staffID := uuid.New()
	repo, events, paid := refundFixture(&buyer, false, 60)
	pay := &fakeTicketPayments{}
	uc := NewRefundUseCase(repo, events, pay, &fakePerms{allow: true})
	staff := Actor{UserID: &staffID, Role: domain.RoleRestaurant}

	if _, err := uc.Refund(context.Background(), staff, paid.ID, RefundInput{IdempotencyKey: "s1"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("staff without override: err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved without an override: refundN = %d", pay.refundN)
	}

	tk, err := uc.Refund(context.Background(), staff, paid.ID, RefundInput{IdempotencyKey: "s1", Override: true})
	if err != nil {
		t.Fatalf("staff override: %v", err)
	}
	if tk.Status != domain.TicketRefunded || pay.refundN != 1 {
		t.Fatalf("status=%s refundN=%d, want refunded/1", tk.Status, pay.refundN)
	}
}

// TestRefundStaffWithoutPermission: a staff-role caller who does NOT hold
// payment.refund at this ticket's restaurant gets no override at all — they are
// judged as a non-owner guest, so cross-venue staff cannot refund a stranger's
// ticket.
func TestRefundStaffWithoutPermission(t *testing.T) {
	buyer := uuid.New()
	otherStaff := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 60)
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{allow: false})
	_, err := uc.Refund(context.Background(), Actor{UserID: &otherStaff, Role: domain.RoleRestaurant}, paid.ID,
		RefundInput{IdempotencyKey: "s2", Override: true})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved for an unauthorized staff actor: refundN = %d", pay.refundN)
	}
}

// TestRefundNotByAnotherGuest: a logged-in stranger cannot refund somebody
// else's ticket even when the policy would allow the OWNER to.
func TestRefundNotByAnotherGuest(t *testing.T) {
	buyer := uuid.New()
	stranger := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 60)
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	_, err := uc.Refund(context.Background(), Actor{UserID: &stranger}, paid.ID, RefundInput{IdempotencyKey: "x"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved for a stranger: refundN = %d", pay.refundN)
	}
}

// TestRefundAccountlessTicketNeedsVenue: an account-less (guest checkout)
// ticket has no self-refund path — the ticket id is not proof of ownership, so
// only the venue can refund it.
func TestRefundAccountlessTicketNeedsVenue(t *testing.T) {
	repo, events, paid := refundFixture(nil, true, 60)
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	_, err := uc.Refund(context.Background(), Actor{}, paid.ID, RefundInput{IdempotencyKey: "a1"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if pay.refundN != 0 {
		t.Fatalf("money moved for an anonymous caller: refundN = %d", pay.refundN)
	}

	// The venue still can, within the policy, without any override.
	staffID := uuid.New()
	uc = NewRefundUseCase(repo, events, pay, &fakePerms{allow: true})
	if _, err := uc.Refund(context.Background(), Actor{UserID: &staffID, Role: domain.RoleRestaurant}, paid.ID,
		RefundInput{IdempotencyKey: "a2"}); err != nil {
		t.Fatalf("venue refund of an account-less ticket: %v", err)
	}
}

// TestRefundUsesTicketSnapshotNotEvent: the venue tightened its policy AFTER
// the sale (the event is now non-refundable), but the ticket was bought while
// refunds were open — the terms promise the guest the rules at purchase time,
// so the snapshot wins.
func TestRefundUsesTicketSnapshotNotEvent(t *testing.T) {
	buyer := uuid.New()
	repo, events, paid := refundFixture(&buyer, true, 60)
	for _, e := range events.byID {
		e.TicketsRefundable = false
		e.TicketRefundCutoffMinutes = 30 * 24 * 60
	}
	pay := &fakeTicketPayments{}

	uc := NewRefundUseCase(repo, events, pay, &fakePerms{})
	if _, err := uc.Refund(context.Background(), Actor{UserID: &buyer}, paid.ID, RefundInput{IdempotencyKey: "sn"}); err != nil {
		t.Fatalf("snapshot must survive a later policy change: %v", err)
	}
	if pay.refundN != 1 {
		t.Fatalf("refundN = %d, want 1", pay.refundN)
	}
}

// TestRefundPolicySnapshotFrozenAtPurchase: the purchase path copies the
// event's rules onto the ticket, which is what makes the promise above
// keepable.
func TestRefundPolicySnapshotFrozenAtPurchase(t *testing.T) {
	event := ticketedEvent(nil, 20000)
	event.TicketsRefundable = true
	event.TicketRefundCutoffMinutes = 180
	repo := newFakeTicketRepo()
	uc := NewPurchaseUseCase(repo, newFakeEvents(event), &fakeTicketPayments{}, &fakePaymentReader{}, fakeTx{})

	res, err := uc.Purchase(context.Background(), Actor{}, PurchaseInput{
		EventID: event.ID, Quantity: 1, GuestPhone: "+77010000000", IdempotencyKey: "p1",
	})
	if err != nil {
		t.Fatalf("Purchase: %v", err)
	}
	// The PERSISTED row is what matters — an in-memory copy proves nothing.
	stored, err := repo.GetByID(context.Background(), res.Ticket.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got := stored.RefundPolicy(); got != (domain.TicketRefundPolicy{Refundable: true, CutoffMinutes: 180}) {
		t.Fatalf("stored snapshot = %+v, want refundable/180", got)
	}
}
