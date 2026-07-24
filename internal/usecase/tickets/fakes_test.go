package tickets

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/payments"
)

// Hand-written fakes (project convention: no mock framework).

type fakeTicketRepo struct {
	mu        sync.Mutex
	byID      map[uuid.UUID]*domain.EventTicket
	createErr error
}

func newFakeTicketRepo() *fakeTicketRepo {
	return &fakeTicketRepo{byID: map[uuid.UUID]*domain.EventTicket{}}
}

func (f *fakeTicketRepo) LockEventForCapacity(context.Context, uuid.UUID) error { return nil }

func (f *fakeTicketRepo) SoldCount(_ context.Context, eventID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sold := 0
	for _, t := range f.byID {
		if t.EventID == eventID && t.Status.HoldsCapacity() {
			sold += t.Quantity
		}
	}
	return sold, nil
}

func (f *fakeTicketRepo) Create(_ context.Context, t *domain.EventTicket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	for _, e := range f.byID {
		if e.EventID == t.EventID && e.PurchaseIdempotencyKey == t.PurchaseIdempotencyKey {
			return domain.ErrAlreadyExists
		}
	}
	cp := *t
	f.byID[t.ID] = &cp
	return nil
}

func (f *fakeTicketRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.EventTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTicketRepo) GetByIdempotencyKey(_ context.Context, eventID uuid.UUID, key string) (*domain.EventTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byID {
		if t.EventID == eventID && t.PurchaseIdempotencyKey == key {
			cp := *t
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeTicketRepo) SetPaymentID(_ context.Context, id, paymentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	t.PaymentID = &paymentID
	return nil
}

func (f *fakeTicketRepo) CompareAndSwapStatus(_ context.Context, id uuid.UUID, from, to domain.TicketStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if t.Status != from {
		return domain.ErrAlreadyExists
	}
	t.Status = to
	t.UpdatedAt = at
	return nil
}

func (f *fakeTicketRepo) ListByEvent(_ context.Context, eventID uuid.UUID, statuses []domain.TicketStatus, _, _ int) ([]domain.EventTicket, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.EventTicket
	for _, t := range f.byID {
		if t.EventID != eventID {
			continue
		}
		if len(statuses) > 0 && !contains(statuses, t.Status) {
			continue
		}
		out = append(out, *t)
	}
	return out, len(out), nil
}

func (f *fakeTicketRepo) ListByUser(_ context.Context, userID uuid.UUID, _, _ int) ([]domain.EventTicket, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.EventTicket
	for _, t := range f.byID {
		if t.UserID != nil && *t.UserID == userID {
			out = append(out, *t)
		}
	}
	return out, len(out), nil
}

func (f *fakeTicketRepo) ListStalePending(_ context.Context, before time.Time, limit int) ([]domain.EventTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.EventTicket
	for _, t := range f.byID {
		if t.Status == domain.TicketPending && t.CreatedAt.Before(before) {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeTicketRepo) Counts(_ context.Context, eventID uuid.UUID) (domain.EventTicketCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := domain.EventTicketCounts{EventID: eventID, Currency: domain.CurrencyKZT}
	for _, t := range f.byID {
		if t.EventID != eventID {
			continue
		}
		switch t.Status {
		case domain.TicketPending:
			c.PendingTickets++
			c.PendingQuantity += t.Quantity
		case domain.TicketPaid:
			c.PaidTickets++
			c.PaidQuantity += t.Quantity
			c.RevenuePaidMinor += t.TotalMinor
		case domain.TicketRefunded:
			c.RefundedTickets++
		case domain.TicketCancelled:
			c.CancelledTickets++
		}
	}
	return c, nil
}

func contains(ss []domain.TicketStatus, s domain.TicketStatus) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

type fakeEvents struct {
	byID map[uuid.UUID]*domain.Event
}

func newFakeEvents(es ...*domain.Event) *fakeEvents {
	m := map[uuid.UUID]*domain.Event{}
	for _, e := range es {
		m[e.ID] = e
	}
	return &fakeEvents{byID: m}
}

func (f *fakeEvents) GetByID(_ context.Context, id uuid.UUID) (*domain.Event, error) {
	e, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

// fakeTicketPayments records the money calls and returns configurable results.
type fakeTicketPayments struct {
	createErr    error
	refundErr    error
	lastCreate   payments.TicketPaymentInput
	lastRefund   payments.TicketRefundInput
	createN      int
	refundN      int
	nextPayment  func(in payments.TicketPaymentInput) *domain.Payment
	refundResult *domain.Payment
}

func (f *fakeTicketPayments) CreateForTicket(_ context.Context, _ payments.Actor, in payments.TicketPaymentInput) (*domain.Payment, error) {
	f.createN++
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.nextPayment != nil {
		return f.nextPayment(in), nil
	}
	id := uuid.New()
	tid := in.EventTicketID
	return &domain.Payment{
		ID: id, EventTicketID: &tid, RestaurantID: in.RestaurantID, UserID: in.UserID,
		Purpose: domain.PurposeTicket, Status: domain.PaymentCreated,
		BaseAmountMinor: in.BaseAmountMinor, AmountMinor: in.BaseAmountMinor, Currency: in.Currency,
	}, nil
}

func (f *fakeTicketPayments) RefundTicket(_ context.Context, _ payments.Actor, in payments.TicketRefundInput) (*domain.Payment, error) {
	f.refundN++
	f.lastRefund = in
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	if f.refundResult != nil {
		return f.refundResult, nil
	}
	return &domain.Payment{ID: in.PaymentID, Status: domain.PaymentRefunded}, nil
}

type fakePerms struct {
	allow bool
	err   error
}

func (f *fakePerms) HasPermission(context.Context, uuid.UUID, uuid.UUID, domain.Permission) (bool, error) {
	return f.allow, f.err
}

// fakeTx runs the closure directly; these usecase tests do not exercise
// rollback (that is covered by the real-Postgres repository tests).
type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (fakeTx) Detach(ctx context.Context) context.Context                         { return ctx }

// helpers

func ptr[T any](v T) *T { return &v }

func nowPlus() time.Time { return time.Now().Add(24 * time.Hour) }

func ticketedEvent(capacity *int, price int64) *domain.Event {
	return &domain.Event{
		ID: uuid.New(), RestaurantID: uuid.New(),
		Status: domain.EventPublished, Ticketed: true, TicketPriceMinor: &price, Capacity: capacity,
		StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(48 * time.Hour),
	}
}
