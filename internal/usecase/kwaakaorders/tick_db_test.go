package kwaakaorders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	"backend-core/internal/infrastructure/postgres/kwaakaorder"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// These tests drive the REAL Tick against a real Postgres: the stage order is
// part of the behaviour, and the fake-repo tests call stages one by one in the
// order the test author picked, which is how the original defect slipped by.

func tickSeed(t *testing.T, pool *pgxpool.Pool) (bookingID, orderID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// Tick works on EVERY due row, so leftovers of other tests would be driven too.
	testdb.Truncate(t, pool, "kwaaka_kitchen_orders")
	rid, bid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id, timezone)
		VALUES ($1,'R','Алматы','₸','kw-1','Asia/Almaty')`, rid); err != nil {
		t.Fatal(err)
	}
	st := time.Now().Add(3 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status)
		VALUES ($1,$2,'G','+7 777 123 45 67','+77771234567',2,$3,$4,'confirmed')`, bid, rid, st, st.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	o := &domain.KitchenOrder{ID: uuid.New(), BookingID: bid, RestaurantID: rid, KwaakaRestaurantID: "kw-1", KwaakaTableID: "T1",
		Status: domain.KitchenOrderSending, Trigger: domain.KitchenTriggerLead, Paid: true, TotalMinor: 1000,
		RequestSnapshot: json.RawMessage(`{"order_id":"x"}`), BookingStartsAt: st, DeadlineAt: time.Now().Add(2 * time.Hour),
		TableHoldUntil: time.Now().Add(5 * time.Hour), NextAttemptAt: &past}
	if _, err := kwaakaorder.NewOrders(pool).Insert(ctx, o); err != nil {
		t.Fatal(err)
	}
	return bid, o.ID
}

func tickWorker(pool *pgxpool.Pool, pos domain.KwaakaOrderPOS, at *time.Time) *Worker {
	w := NewWorker(kwaakaorder.NewOrders(pool), fakeSettings{}, pos, bookingrepo.NewOutbox(pool), sqltx.NewManager(pool),
		Config{Enabled: true, MaxAttempts: 8}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return *at }
	return w
}

// A booking cancelled while its row waits for a retry after 429/401 must end
// `cancelled` with exactly ONE create POST (the refused one), never a second.
func TestTickCancelWhileWaitingRetryAfterRefusalSendsNoSecondCreate(t *testing.T) {
	for _, out := range []domain.PosCallOutcome{domain.PosRateLimit, domain.PosAuth} {
		pool := testdb.Connect(t)
		bid, oid := tickSeed(t, pool)
		repo := kwaakaorder.NewOrders(pool)
		now := time.Now()
		pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: out}, {Outcome: domain.PosCreated, PosOrderID: "K"}},
			cancel: domain.PosCancelResult{Outcome: domain.PosCreated}}
		w := tickWorker(pool, pos, &now)
		w.Tick(context.Background()) // attempt 1: refused, row waits for backoff
		if _, err := pool.Exec(context.Background(), `UPDATE bookings SET status='cancelled' WHERE id=$1`, bid); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		w.Tick(context.Background())
		w.Tick(context.Background())
		r, _ := repo.GetByID(context.Background(), oid)
		if len(pos.creates) != 1 || r.Status != domain.KitchenOrderCancelled {
			t.Errorf("outcome %v: creates=%d cancels=%d status=%s, want 1 create and cancelled", out, len(pos.creates), len(pos.cancels), r.Status)
		}
	}
}

// Timeout (outcome unknown) then the booking is cancelled: no second create,
// the order goes to cancel (404 on the POS side is resolved by cancelOne).
func TestTickCancelAfterTimeoutSendsNoSecondCreate(t *testing.T) {
	pool := testdb.Connect(t)
	bid, oid := tickSeed(t, pool)
	repo := kwaakaorder.NewOrders(pool)
	now := time.Now()
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosUnknown, Message: "transport: timeout"}, {Outcome: domain.PosCreated, PosOrderID: "K"}},
		cancel: domain.PosCancelResult{Outcome: domain.PosRejected}, getErr: domain.ErrNotFound}
	w := tickWorker(pool, pos, &now)
	w.Tick(context.Background())
	if _, err := pool.Exec(context.Background(), `UPDATE bookings SET status='cancelled' WHERE id=$1`, bid); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	w.Tick(context.Background())
	w.Tick(context.Background())
	r, _ := repo.GetByID(context.Background(), oid)
	if len(pos.creates) != 1 || r.Status != domain.KitchenOrderCancelled {
		t.Errorf("creates=%d cancels=%d gets=%d status=%s, want 1 create and cancelled", len(pos.creates), len(pos.cancels), pos.gets, r.Status)
	}
}

// The send budget (MaxAttempts) is spent on timeouts, THEN the booking is
// cancelled. The cancel must get its own budget: at least one real POS request
// (cancel or GET), and no cancel_failed alert for an order the POS never saw.
func TestTickCancelAfterSendAttemptsSpentOnTimeoutsAsksThePOS(t *testing.T) {
	for _, k := range []int{7, 8} {
		t.Run(fmt.Sprintf("timeouts=%d", k), func(t *testing.T) {
			pool := testdb.Connect(t)
			bid, oid := tickSeed(t, pool)
			repo := kwaakaorder.NewOrders(pool)
			now := time.Now()
			pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosUnknown, Message: "transport: timeout"}},
				cancel: domain.PosCancelResult{Outcome: domain.PosRejected}, getErr: domain.ErrNotFound}
			w := tickWorker(pool, pos, &now)
			for i := 0; i < k; i++ {
				if err := w.SendPass(context.Background()); err != nil {
					t.Fatal(err)
				}
				now = now.Add(11 * time.Minute)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE bookings SET status='cancelled' WHERE id=$1`, bid); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				w.Tick(context.Background())
				now = now.Add(time.Minute)
			}
			r, _ := repo.GetByID(context.Background(), oid)
			var alerts int
			_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM booking_outbox WHERE booking_id=$1 AND event_type=$2`,
				bid, domain.EventBookingKitchenOrderAttention).Scan(&alerts)
			if len(pos.cancels)+pos.gets == 0 {
				t.Errorf("status=%s: no cancel and no GET reached the POS", r.Status)
			}
			if r.Status != domain.KitchenOrderCancelled || alerts != 0 || len(pos.creates) != k {
				t.Errorf("status=%s alerts=%d creates=%d cancels=%d gets=%d, want cancelled, 0 alerts, %d creates",
					r.Status, alerts, len(pos.creates), len(pos.cancels), pos.gets, k)
			}
		})
	}
}

// The cancel flag lands while a create POST is in flight and the POST succeeds
// (second instance's CancelSweep). Must end cancelled: exactly 1 create, 1 cancel,
// no alert.
func TestTickCancelFlagDuringSuccessfulPostEndsCancelled(t *testing.T) {
	pool := testdb.Connect(t)
	bid, oid := tickSeed(t, pool)
	repo := kwaakaorder.NewOrders(pool)
	now := time.Now()
	pos := &hookPOS{fakePOS: fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K1"}},
		cancel: domain.PosCancelResult{Outcome: domain.PosCreated}}}
	w := tickWorker(pool, pos, &now)
	other := tickWorker(pool, pos, &now)
	pos.onCreate = func() {
		if _, err := pool.Exec(context.Background(), `UPDATE bookings SET status='cancelled' WHERE id=$1`, bid); err != nil {
			t.Error(err)
		}
		if err := other.CancelSweep(context.Background()); err != nil {
			t.Error(err)
		}
	}
	w.Tick(context.Background())
	r, _ := repo.GetByID(context.Background(), oid)
	if r.Status != domain.KitchenOrderCancelling || r.CancelRequestedAt == nil || r.KwaakaOrderID == nil || *r.KwaakaOrderID != "K1" {
		t.Fatalf("after the POST: status=%s flag=%v kwaaka_id=%v, want cancelling with the flag and K1 kept", r.Status, r.CancelRequestedAt != nil, r.KwaakaOrderID)
	}
	now = now.Add(time.Minute)
	w.Tick(context.Background())
	r, _ = repo.GetByID(context.Background(), oid)
	var alerts int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM booking_outbox WHERE booking_id=$1 AND event_type=$2`,
		bid, domain.EventBookingKitchenOrderAttention).Scan(&alerts)
	if r.Status != domain.KitchenOrderCancelled || len(pos.creates) != 1 || len(pos.cancels) != 1 || alerts != 0 {
		t.Errorf("status=%s creates=%d cancels=%d alerts=%d, want cancelled, 1, 1, 0", r.Status, len(pos.creates), len(pos.cancels), alerts)
	}
}

// hookPOS runs onCreate after a create POST has been answered, i.e. while the
// caller of CreateTableOrder has not yet committed anything.
type hookPOS struct {
	fakePOS
	onCreate func()
}

func (p *hookPOS) CreateTableOrder(ctx context.Context, rid string, s domain.KitchenSnapshot) domain.PosCreateResult {
	r := p.fakePOS.CreateTableOrder(ctx, rid, s)
	if p.onCreate != nil {
		p.onCreate()
	}
	return r
}
