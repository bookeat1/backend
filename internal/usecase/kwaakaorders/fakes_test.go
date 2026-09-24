package kwaakaorders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (fakeTx) Detach(ctx context.Context) context.Context                         { return ctx }

type fakeOutbox struct{ events []*domain.BookingOutboxEvent }

func (f *fakeOutbox) Create(_ context.Context, e *domain.BookingOutboxEvent) error {
	f.events = append(f.events, e)
	return nil
}

// fakeOrders is an in-memory KitchenOrderRepository holding one row.
type fakeOrders struct {
	mu        sync.Mutex
	row       *domain.KitchenOrder
	sub       *domain.KitchenClaimSubject
	inserted  []*domain.KitchenOrder
	loads     domain.KitchenTableLoad
	cancelled []domain.KitchenOrder
}

func (f *fakeOrders) ListCandidates(context.Context, time.Time, int) ([]domain.KitchenCandidate, error) {
	if f.sub == nil {
		return nil, nil
	}
	return []domain.KitchenCandidate{{BookingID: f.sub.BookingID, RestaurantID: f.sub.RestaurantID, KwaakaRestaurantID: f.sub.KwaakaID}}, nil
}
func (f *fakeOrders) GetClaimSubject(context.Context, uuid.UUID) (*domain.KitchenClaimSubject, error) {
	return f.sub, nil
}
func (f *fakeOrders) LockClaimSubject(context.Context, uuid.UUID) (*domain.KitchenClaimSubject, error) {
	return f.sub, nil
}
func (f *fakeOrders) Insert(_ context.Context, o *domain.KitchenOrder) (bool, error) {
	for _, e := range f.inserted {
		if e.BookingID == o.BookingID {
			return false, nil
		}
	}
	c := *o
	f.inserted = append(f.inserted, &c)
	f.row = &c
	return true, nil
}
func (f *fakeOrders) GetByID(_ context.Context, id uuid.UUID) (*domain.KitchenOrder, error) {
	if f.row == nil || f.row.ID != id {
		return nil, domain.ErrNotFound
	}
	c := *f.row
	return &c, nil
}
func (f *fakeOrders) GetByBookingID(context.Context, uuid.UUID) (*domain.KitchenOrder, error) {
	c := *f.row
	return &c, nil
}
func (f *fakeOrders) GetByPosOrderID(_ context.Context, id string) (*domain.KitchenOrder, error) {
	if f.row != nil && f.row.KwaakaOrderID != nil && *f.row.KwaakaOrderID == id {
		c := *f.row
		return &c, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeOrders) ListByBookingIDs(context.Context, []uuid.UUID) (map[uuid.UUID]domain.KitchenOrderView, error) {
	return nil, nil
}
func (f *fakeOrders) LeaseDue(context.Context, time.Time, time.Duration, int) ([]domain.KitchenOrder, error) {
	return nil, nil
}
func (f *fakeOrders) CompareAndSet(_ context.Context, o *domain.KitchenOrder, want domain.KitchenOrderStatus, attempts int) (bool, error) {
	if f.row.Status != want || f.row.Attempts != attempts {
		return false, nil
	}
	c := *o
	f.row = &c
	return true, nil
}
func (f *fakeOrders) TableLoads(context.Context, uuid.UUID, time.Time) (domain.KitchenTableLoad, error) {
	return f.loads, nil
}
func (f *fakeOrders) ListCancelledPending(context.Context, int) ([]domain.KitchenOrder, error) {
	if f.row != nil && f.row.CancelRequestedAt == nil {
		return []domain.KitchenOrder{*f.row}, nil
	}
	return nil, nil
}
func (f *fakeOrders) ListRescheduled(context.Context, int) ([]domain.KitchenReschedule, error) {
	return nil, nil
}
func (f *fakeOrders) ListPollDue(context.Context, time.Time, time.Time, int) ([]domain.KitchenOrder, error) {
	return nil, nil
}

type fakeSettings struct{ s *domain.KwaakaOrderSettings }

func (f fakeSettings) Get(context.Context, uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	return f.s, nil
}
func (f fakeSettings) LockForUpdate(context.Context, uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	return f.s, nil
}
func (f fakeSettings) Save(context.Context, *domain.KwaakaOrderSettings) error { return nil }

// fakePOS records every call; results are scripted.
type fakePOS struct {
	creates  []domain.KitchenSnapshot
	cancels  []string
	gets     int
	lists    int
	create   []domain.PosCreateResult // consumed in order, last repeats
	cancel   domain.PosCancelResult
	getOrder *domain.PosOrder
	getErr   error
	listOut  []domain.PosOrder
	listErr  error
}

func (f *fakePOS) CreateTableOrder(_ context.Context, _ string, s domain.KitchenSnapshot) domain.PosCreateResult {
	f.creates = append(f.creates, s)
	i := len(f.creates) - 1
	if i >= len(f.create) {
		i = len(f.create) - 1
	}
	return f.create[i]
}
func (f *fakePOS) CancelTableOrder(_ context.Context, rid, id, _ string) domain.PosCancelResult {
	f.cancels = append(f.cancels, rid+"|"+id)
	return f.cancel
}
func (f *fakePOS) GetOrder(context.Context, string, string) (*domain.PosOrder, error) {
	f.gets++
	return f.getOrder, f.getErr
}
func (f *fakePOS) ListOrdersByTables(context.Context, string, []string) ([]domain.PosOrder, error) {
	f.lists++
	return f.listOut, f.listErr
}
func (f *fakePOS) GetTables(context.Context, string) ([]domain.PosTable, error) {
	return nil, errors.New("n/a")
}

func newTestWorker(o *fakeOrders, st fakeSettings, pos *fakePOS, ob *fakeOutbox, now time.Time) *Worker {
	w := NewWorker(o, st, pos, ob, fakeTx{}, Config{Enabled: true, MaxAttempts: 5}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return now }
	return w
}

type fakeHooks struct {
	ev       []domain.KwaakaWebhookEvent
	outcome  map[uuid.UUID]string
	deferred map[uuid.UUID]time.Time
}

func newFakeHooks(evs ...domain.KwaakaWebhookEvent) *fakeHooks {
	return &fakeHooks{ev: evs, outcome: map[uuid.UUID]string{}, deferred: map[uuid.UUID]time.Time{}}
}
func (f *fakeHooks) Insert(context.Context, *domain.KwaakaWebhookEvent) (bool, error) {
	return true, nil
}
func (f *fakeHooks) LockDue(context.Context, time.Time, int) ([]domain.KwaakaWebhookEvent, error) {
	var out []domain.KwaakaWebhookEvent
	for _, e := range f.ev {
		if _, done := f.outcome[e.ID]; !done {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeHooks) Finish(_ context.Context, id uuid.UUID, o string, _ *string, _ *uuid.UUID) error {
	f.outcome[id] = o
	return nil
}
func (f *fakeHooks) Defer(_ context.Context, id uuid.UUID, next time.Time) error {
	f.deferred[id] = next
	return nil
}
func (f *fakeHooks) PruneProcessed(context.Context, time.Time, int) (int, error) { return 0, nil }
