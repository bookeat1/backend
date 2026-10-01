package kwaakaorders

import (
	"context"
	"encoding/json"
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
