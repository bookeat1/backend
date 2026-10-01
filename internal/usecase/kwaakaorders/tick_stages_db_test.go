package kwaakaorders

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// These tests run the REAL Tick, every stage, against a real Postgres with the
// REAL repositories (orders, settings, webhook inbox) and a fake POS. They
// exist because of a prod incident: the poll stage's query could not be
// planned (`timestamptz > interval`), and the worker logged "kwaaka orders
// stage failed" every 30 s on a prod where sending is switched off. Nothing in
// the fake-repository tests could see that. Every test here fails if ANY stage
// logs an error.

// errLog is a slog handler target that remembers whether anything at ERROR
// level (a failed stage, a failed apply) was logged.
type errLog struct{ buf bytes.Buffer }

func (l *errLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelError}))
}

func (l *errLog) mustBeEmpty(t *testing.T) {
	t.Helper()
	if l.buf.Len() > 0 {
		t.Fatalf("worker logged errors:\n%s", l.buf.String())
	}
}

// realWorker wires the production repositories; only the POS is fake.
func realWorker(pool *pgxpool.Pool, pos domain.KwaakaOrderPOS, enabled bool, pollEvery time.Duration, at *time.Time, lg *slog.Logger) *Worker {
	w := NewWorker(kwaakaorder.NewOrders(pool), kwaakaorder.NewSettings(pool), pos, bookingrepo.NewOutbox(pool),
		sqltx.NewManager(pool), Config{Enabled: enabled, MaxAttempts: 8}, lg).
		WithInbox(kwaakaorder.NewWebhooks(pool), pollEvery)
	w.now = func() time.Time { return *at }
	return w
}

func cleanKwaakaTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	testdb.Truncate(t, pool, "kwaaka_kitchen_orders", "kwaaka_webhook_events")
}

// Prod state: Kwaaka keys present (so the worker starts), KWAAKA_ORDERS_ENABLED
// =false, no venue enabled, tables empty. Every stage must run clean and touch
// nothing, and the POS must not be called.
func TestTickFlagOffEmptyTablesEveryStageIsClean(t *testing.T) {
	pool := testdb.Connect(t)
	cleanKwaakaTables(t, pool)
	testdb.Truncate(t, pool, "restaurant_kwaaka_order_settings")
	var lg errLog
	pos := &fakePOS{}
	now := time.Now()
	w := realWorker(pool, pos, false, 5*time.Minute, &now, lg.logger())

	w.Tick(context.Background())
	w.Tick(context.Background())

	lg.mustBeEmpty(t)
	if len(pos.creates)+len(pos.cancels)+pos.gets+pos.lists != 0 {
		t.Fatalf("POS was called with nothing to do: %+v", pos)
	}
}

// Same prod flag state, but rows that DO exist: the poll stage must reconcile a
// sent order through the real repositories, and the inbox stage must apply a
// stored webhook. Sending of new orders stays off.
func TestTickFlagOffPollsSentOrderAndAppliesWebhook(t *testing.T) {
	pool := testdb.Connect(t)
	cleanKwaakaTables(t, pool)
	ctx := context.Background()
	_, orderID := tickSeed(t, pool)
	repo := kwaakaorder.NewOrders(pool)
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET status='sent', next_attempt_at=NULL,
		sent_at=now() - interval '1 minute', kwaaka_order_id='K-9' WHERE id=$1`, orderID); err != nil {
		t.Fatal(err)
	}

	var lg errLog
	pos := &fakePOS{getOrder: &domain.PosOrder{ID: "K-9", Raw: "PRINTED", State: domain.PosStateBillPrinted}}
	now := time.Now()
	w := realWorker(pool, pos, false, 5*time.Minute, &now, lg.logger())

	w.Tick(ctx)
	lg.mustBeEmpty(t)
	got, err := repo.GetByID(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if pos.gets != 1 || got.PosState == nil || *got.PosState != domain.PosStateBillPrinted ||
		got.PosStatusSource == nil || *got.PosStatusSource != "poll" || got.Status != domain.KitchenOrderSent {
		t.Fatalf("poll did not reconcile: gets=%d order=%+v", pos.gets, got)
	}

	// Polled just now: the next tick must not call the POS again.
	w.Tick(ctx)
	if pos.gets != 1 {
		t.Fatalf("recently polled order was polled again: gets=%d", pos.gets)
	}

	// A closing webhook through the real inbox.
	hooks := kwaakaorder.NewWebhooks(pool)
	if ok, err := hooks.Insert(ctx, &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookOrderStatus, DedupKey: uuid.NewString(),
		Body: []byte(`{"orderId":"K-9","status":"CLOSED"}`)}); err != nil || !ok {
		t.Fatalf("insert webhook: %v %v", ok, err)
	}
	w.Tick(ctx)
	lg.mustBeEmpty(t)
	got, _ = repo.GetByID(ctx, orderID)
	if got.PosState == nil || *got.PosState != domain.PosStateClosed || got.TableReleasedAt == nil {
		t.Fatalf("webhook not applied: %+v", got)
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kwaaka_webhook_events WHERE processed_at IS NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("unprocessed webhooks=%d err=%v", open, err)
	}
	if len(pos.creates) != 0 {
		t.Fatal("a create POST went out with sending switched off")
	}
}

// Sending ON, real settings: claim (ListCandidates, GetClaimSubject, the locked
// subject, TableLoads, Insert) -> send -> poll -> webhook, all through Tick.
// TableLoads carried the same interval defect as the poll query and would have
// failed every claim the day the flag was switched on.
func TestTickFlagOnClaimSendPollEndToEnd(t *testing.T) {
	pool := testdb.Connect(t)
	cleanKwaakaTables(t, pool)
	ctx := context.Background()
	rid, bid := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	exec(`INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id, timezone)
	      VALUES ($1,'R','Алматы','₸','kw-e2e','Asia/Almaty')`, rid)
	menu := uuid.New()
	exec(`INSERT INTO menu_items (id, restaurant_id, name, price, kwaaka_product_id) VALUES ($1,$2,'Плов',2500,'kw-prod-1')`, menu, rid)
	if err := kwaakaorder.NewSettings(pool).Save(ctx, &domain.KwaakaOrderSettings{RestaurantID: rid, KwaakaRestaurantID: "kw-e2e",
		OrdersEnabled: true, Pool: []domain.KwaakaPoolTable{{KwaakaTableID: "T1", Position: 1, Label: "1"}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the booking is confirmed AFTER the venue was enabled
	st := time.Now().Add(30 * time.Minute)
	exec(`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status, confirmed_at)
	      VALUES ($1,$2,'G','+7 777 123 45 67','+77771234567',2,$3,$4,'confirmed', now())`, bid, rid, st, st.Add(2*time.Hour))
	exec(`INSERT INTO booking_items (id, booking_id, menu_item_id, item_name, item_price_minor, quantity, status)
	      VALUES ($1,$2,$3,'Плов',250000,2,'pending')`, uuid.New(), bid, menu)
	exec(`INSERT INTO payments (id, booking_id, restaurant_id, provider, purpose, status, amount_minor, base_amount_minor,
	        idempotency_key, captured_at) VALUES ($1,$2,$3,'tiptoppay','preorder','captured',500000,500000,$4, now())`,
		uuid.New(), bid, rid, uuid.NewString())

	var lg errLog
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K-E2E"}},
		getOrder: &domain.PosOrder{ID: "K-E2E", Raw: "OPEN", State: domain.PosStateOpen}}
	now := time.Now().Add(time.Second)
	w := realWorker(pool, pos, true, time.Minute, &now, lg.logger())
	repo := kwaakaorder.NewOrders(pool)

	w.Tick(ctx)
	lg.mustBeEmpty(t)
	o, err := repo.GetByBookingID(ctx, bid)
	if err != nil {
		t.Fatalf("claim did not create the order: %v", err)
	}
	if len(pos.creates) != 1 || o.Status != domain.KitchenOrderSent || o.KwaakaTableID != "T1" || o.KwaakaOrderID == nil || *o.KwaakaOrderID != "K-E2E" {
		t.Fatalf("after claim+send: creates=%d order=%+v", len(pos.creates), o)
	}

	// A sent order with no status yet is poll-due at once: the first Tick already
	// asked the POS and recorded `open`.
	o, _ = repo.GetByID(ctx, o.ID)
	if pos.gets != 1 || o.PosState == nil || *o.PosState != domain.PosStateOpen {
		t.Fatalf("poll after send: gets=%d order=%+v", pos.gets, o)
	}
	now = now.Add(2 * time.Minute) // past the poll interval: asked again, same state is stale, nothing changes
	w.Tick(ctx)
	lg.mustBeEmpty(t)
	o, _ = repo.GetByID(ctx, o.ID)
	if pos.gets != 2 || o.PosState == nil || *o.PosState != domain.PosStateOpen || o.Status != domain.KitchenOrderSent {
		t.Fatalf("repeat poll: gets=%d order=%+v", pos.gets, o)
	}

	// Table load seen by the real TableLoads: one live order on T1.
	load, err := repo.TableLoads(ctx, rid, now)
	if err != nil || load.Live["T1"] != 1 {
		t.Fatalf("TableLoads: %+v %v", load, err)
	}

	// Booking cancelled -> cancel stage flags it, send stage posts the cancel, poll must stay quiet.
	exec(`UPDATE bookings SET status='cancelled' WHERE id=$1`, bid)
	pos.cancel = domain.PosCancelResult{Outcome: domain.PosCreated}
	now = now.Add(time.Minute)
	w.Tick(ctx)
	lg.mustBeEmpty(t)
	o, _ = repo.GetByID(ctx, o.ID)
	if len(pos.cancels) != 1 || !strings.HasPrefix(pos.cancels[0], "kw-e2e|") {
		t.Fatalf("cancel POSTs: %v (order %+v)", pos.cancels, o)
	}
}
