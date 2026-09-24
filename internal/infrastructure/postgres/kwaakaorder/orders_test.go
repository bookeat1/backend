package kwaakaorder

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

type seeded struct{ restaurant, booking uuid.UUID }

func seed(t *testing.T, pool *pgxpool.Pool, startsIn time.Duration, status string) seeded {
	t.Helper()
	ctx := context.Background()
	s := seeded{uuid.New(), uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id, timezone)
		VALUES ($1,'R','Алматы','₸','kw-1','Asia/Almaty')`, s.restaurant); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	starts := time.Now().Add(startsIn)
	if _, err := pool.Exec(ctx, `INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status)
		VALUES ($1,$2,'Гость','+7 777 123 45 67','+77771234567',2,$3,$4,$5)`,
		s.booking, s.restaurant, starts, starts.Add(2*time.Hour), status); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return s
}

func newOrder(s seeded, table string) *domain.KitchenOrder {
	past := time.Now().Add(-time.Minute)
	return &domain.KitchenOrder{
		BookingID: s.booking, RestaurantID: s.restaurant, KwaakaRestaurantID: "kw-1", KwaakaTableID: table,
		Status: domain.KitchenOrderSending, Trigger: domain.KitchenTriggerLead, Paid: true, TotalMinor: 1000,
		RequestSnapshot: json.RawMessage(`{}`), BookingStartsAt: time.Now(), DeadlineAt: time.Now().Add(time.Hour),
		TableHoldUntil: time.Now().Add(3 * time.Hour), NextAttemptAt: &past,
	}
}

func TestInsertIsIdempotentPerBooking(t *testing.T) {
	pool := testdb.Connect(t)
	s := seed(t, pool, time.Hour, "confirmed")
	repo := NewOrders(pool)
	ctx := context.Background()
	created, err := repo.Insert(ctx, newOrder(s, "T1"))
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}
	created, err = repo.Insert(ctx, newOrder(s, "T2"))
	if err != nil || created {
		t.Fatalf("second insert must be a silent no-op: created=%v err=%v", created, err)
	}
	got, err := repo.GetByBookingID(ctx, s.booking)
	if err != nil || got.KwaakaTableID != "T1" {
		t.Fatalf("stored row: %+v err=%v", got, err)
	}
}

func TestLeaseDueParallelTakesEachRowOnce(t *testing.T) {
	pool := testdb.Connect(t)
	repo := NewOrders(pool)
	ctx := context.Background()
	const n = 6
	for i := 0; i < n; i++ {
		s := seed(t, pool, time.Hour, "confirmed")
		if _, err := repo.Insert(ctx, newOrder(s, "T1")); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := repo.LeaseDue(ctx, time.Now(), 2*time.Minute, 3)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, o := range rows {
				seen[o.ID]++
				if o.Attempts != 1 {
					t.Errorf("attempts after lease = %d", o.Attempts)
				}
			}
		}()
	}
	wg.Wait()
	for id, c := range seen {
		if c != 1 {
			t.Errorf("order %s leased %d times", id, c)
		}
	}
	// Leased rows are not due again until the lease lapses.
	again, err := repo.LeaseDue(ctx, time.Now(), 2*time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range again {
		if seen[o.ID] > 0 {
			t.Errorf("order %s re-leased inside its lease", o.ID)
		}
	}
}

func TestCompareAndSetLosesToNewerWriter(t *testing.T) {
	pool := testdb.Connect(t)
	repo := NewOrders(pool)
	ctx := context.Background()
	s := seed(t, pool, time.Hour, "confirmed")
	if _, err := repo.Insert(ctx, newOrder(s, "T1")); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseDue(ctx, time.Now(), time.Minute, 10)
	if err != nil || len(leased) == 0 {
		t.Fatalf("lease: %v %d", err, len(leased))
	}
	var o domain.KitchenOrder
	for _, l := range leased {
		if l.BookingID == s.booking {
			o = l
		}
	}
	o.Status = domain.KitchenOrderSent
	ok, err := repo.CompareAndSet(ctx, &o, domain.KitchenOrderSending, o.Attempts)
	if err != nil || !ok {
		t.Fatalf("first CAS: %v %v", ok, err)
	}
	o.Status = domain.KitchenOrderFailed
	ok, err = repo.CompareAndSet(ctx, &o, domain.KitchenOrderSending, o.Attempts)
	if err != nil || ok {
		t.Fatalf("stale CAS must not swap: %v %v", ok, err)
	}
	got, _ := repo.GetByID(ctx, o.ID)
	if got.Status != domain.KitchenOrderSent {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestSettingsSaveAndEnabledAt(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	s := seed(t, pool, time.Hour, "confirmed")
	repo := NewSettings(pool)
	set := &domain.KwaakaOrderSettings{RestaurantID: s.restaurant, KwaakaRestaurantID: "kw-1",
		Pool: []domain.KwaakaPoolTable{{KwaakaTableID: "B", Position: 2, Label: "b"}, {KwaakaTableID: "A", Position: 1, Label: "a"}}}
	if err := repo.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	if set.EnabledAt != nil {
		t.Fatal("disabled settings must have no enabled_at")
	}
	set.OrdersEnabled = true
	if err := repo.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	first := *set.EnabledAt
	set.Pool = set.Pool[:1] // replaced wholesale
	time.Sleep(5 * time.Millisecond)
	if err := repo.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	if !set.EnabledAt.Equal(first) {
		t.Fatal("enabled_at must move only on false→true")
	}
	got, err := repo.Get(ctx, s.restaurant)
	if err != nil || len(got.Pool) != 1 || got.Pool[0].KwaakaTableID != "B" {
		t.Fatalf("pool after replace: %+v %v", got, err)
	}
	set.OrdersEnabled = false
	_ = repo.Save(ctx, set)
	set.OrdersEnabled = true
	_ = repo.Save(ctx, set)
	if !set.EnabledAt.After(first) {
		t.Fatal("enabled_at must be refreshed on re-enable")
	}
}

func TestWebhookInboxDedupAndPrune(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := NewWebhooks(pool)
	key := uuid.NewString()
	ok, err := repo.Insert(ctx, &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookOrderStatus, DedupKey: key, Body: []byte(`{}`)})
	if err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	ok, err = repo.Insert(ctx, &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookOrderStatus, DedupKey: key, Body: []byte(`{}`)})
	if err != nil || ok {
		t.Fatalf("duplicate must be a silent no-op: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_webhook_events SET processed_at = now() - interval '40 days' WHERE dedup_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	n, err := repo.PruneProcessed(ctx, time.Now().Add(-30*24*time.Hour), 100)
	if err != nil || n < 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
}
