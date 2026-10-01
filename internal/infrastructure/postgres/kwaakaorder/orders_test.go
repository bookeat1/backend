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

// LeaseDue writes ahead "an attempt is in flight" (outcome_unknown=true in the
// DB) for a sending row, while handing the caller the value from BEFORE the
// lease. A worker killed mid-POST therefore leaves the mark behind.
func TestLeaseDueWritesAheadOutcomeUnknown(t *testing.T) {
	pool := testdb.Connect(t)
	repo := NewOrders(pool)
	ctx := context.Background()
	find := func(rows []domain.KitchenOrder, id uuid.UUID) *domain.KitchenOrder {
		for i := range rows {
			if rows[i].ID == id {
				return &rows[i]
			}
		}
		return nil
	}

	s := seed(t, pool, time.Hour, "confirmed")
	o := newOrder(s, "T1")
	if _, err := repo.Insert(ctx, o); err != nil {
		t.Fatal(err)
	}
	// attempt 1: caller sees "no earlier uncertainty", DB already says "in flight".
	rows, err := repo.LeaseDue(ctx, time.Now(), time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	l1 := find(rows, o.ID)
	if l1 == nil || l1.Attempts != 1 || l1.OutcomeUnknown {
		t.Fatalf("first lease: %+v", l1)
	}
	stored, _ := repo.GetByID(ctx, o.ID)
	if !stored.OutcomeUnknown || stored.Attempts != 1 {
		t.Fatalf("write-ahead missing in DB: unknown=%v attempts=%d", stored.OutcomeUnknown, stored.Attempts)
	}
	// the worker "dies"; once the lease lapses the next attempt sees the mark.
	rows, err = repo.LeaseDue(ctx, time.Now().Add(2*time.Minute), time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	l2 := find(rows, o.ID)
	if l2 == nil || l2.Attempts != 2 || !l2.OutcomeUnknown {
		t.Fatalf("restart lease must carry outcome_unknown=true: %+v", l2)
	}

	// A definitive negative (401/429) written back via CAS clears the mark.
	l2.OutcomeUnknown = false
	next := time.Now().Add(-time.Second)
	l2.NextAttemptAt = &next
	if ok, err := repo.CompareAndSet(ctx, l2, domain.KitchenOrderSending, l2.Attempts, time.Now()); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	rows, _ = repo.LeaseDue(ctx, time.Now(), time.Minute, 100)
	if l3 := find(rows, o.ID); l3 == nil || l3.OutcomeUnknown {
		t.Fatalf("after a cleared mark the next lease must report false: %+v", l3)
	}

	// cancelling rows are not marked: a repeated cancel is safe.
	s2 := seed(t, pool, time.Hour, "confirmed")
	c := newOrder(s2, "T2")
	c.Status = domain.KitchenOrderCancelling
	if _, err := repo.Insert(ctx, c); err != nil {
		t.Fatal(err)
	}
	rows, _ = repo.LeaseDue(ctx, time.Now(), time.Minute, 100)
	if lc := find(rows, c.ID); lc == nil || lc.OutcomeUnknown {
		t.Fatalf("cancelling lease: %+v", lc)
	}
	if st, _ := repo.GetByID(ctx, c.ID); st.OutcomeUnknown {
		t.Fatal("cancelling row must not get the write-ahead mark")
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
	ok, err := repo.CompareAndSet(ctx, &o, domain.KitchenOrderSending, o.Attempts, time.Now())
	if err != nil || !ok {
		t.Fatalf("first CAS: %v %v", ok, err)
	}
	o.Status = domain.KitchenOrderFailed
	ok, err = repo.CompareAndSet(ctx, &o, domain.KitchenOrderSending, o.Attempts, time.Now())
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

// A caller holding a copy of the row from BEFORE another worker flagged the
// cancel must not erase the flag on write-back (e.g. the 429 retry commit).
func TestCompareAndSetKeepsCancelFlagSetByAnotherWorker(t *testing.T) {
	pool := testdb.Connect(t)
	s := seed(t, pool, time.Hour, "confirmed")
	repo := NewOrders(pool)
	ctx := context.Background()
	if _, err := repo.Insert(ctx, newOrder(s, "T1")); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.GetByBookingID(ctx, s.booking) // copy without the flag
	if err != nil {
		t.Fatal(err)
	}
	flagged := *stale
	at := time.Now().Truncate(time.Microsecond)
	reason := "booking_cancelled"
	flagged.CancelRequestedAt, flagged.CancelReason = &at, &reason
	if ok, err := repo.CompareAndSet(ctx, &flagged, domain.KitchenOrderSending, stale.Attempts, time.Now()); err != nil || !ok {
		t.Fatalf("flag write: ok=%v err=%v", ok, err)
	}
	next := time.Now().Add(time.Minute)
	stale.NextAttemptAt = &next // what retryLater does with its stale copy
	if ok, err := repo.CompareAndSet(ctx, stale, domain.KitchenOrderSending, stale.Attempts, time.Now()); err != nil || !ok {
		t.Fatalf("stale write: ok=%v err=%v", ok, err)
	}
	got, _ := repo.GetByBookingID(ctx, s.booking)
	if got.CancelRequestedAt == nil || got.CancelReason == nil || *got.CancelReason != reason {
		t.Fatalf("cancel flag erased by a stale write: %+v", got)
	}
}

// A stale copy saying `sent` over a row that already carries a cancel flag must
// land as `cancelling`, due now: `sent` + flag would never be cancelled.
func TestCompareAndSetSentOverCancelFlagBecomesCancelling(t *testing.T) {
	pool := testdb.Connect(t)
	s := seed(t, pool, time.Hour, "confirmed")
	repo := NewOrders(pool)
	ctx := context.Background()
	if _, err := repo.Insert(ctx, newOrder(s, "T1")); err != nil {
		t.Fatal(err)
	}
	stale, _ := repo.GetByBookingID(ctx, s.booking)
	flagged := *stale
	at := time.Now().Add(-time.Second)
	flagged.CancelRequestedAt = &at
	if ok, err := repo.CompareAndSet(ctx, &flagged, domain.KitchenOrderSending, stale.Attempts, time.Now()); err != nil || !ok {
		t.Fatalf("flag write: ok=%v err=%v", ok, err)
	}
	now := time.Now()
	stale.Status, stale.NextAttemptAt, stale.SentAt = domain.KitchenOrderSent, nil, &now
	// A worker clock far from the DB clock: next_attempt_at must follow the worker.
	workerNow := time.Now().Add(-6 * time.Hour).Truncate(time.Microsecond)
	stale.Attempts = 5
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET attempts = 5 WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.CompareAndSet(ctx, stale, domain.KitchenOrderSending, 5, workerNow); err != nil || !ok {
		t.Fatalf("sent write: ok=%v err=%v", ok, err)
	}
	got, _ := repo.GetByBookingID(ctx, s.booking)
	if got.Status != domain.KitchenOrderCancelling || got.NextAttemptAt == nil || got.CancelRequestedAt == nil {
		t.Fatalf("status=%s next=%v flag=%v, want cancelling/due/set", got.Status, got.NextAttemptAt, got.CancelRequestedAt)
	}
	if !got.NextAttemptAt.Equal(workerNow) {
		t.Errorf("next_attempt_at=%v, want the worker clock %v (not the DB clock)", got.NextAttemptAt, workerNow)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts=%d, want 0: entering cancelling starts a fresh budget", got.Attempts)
	}
}

// Entering `cancelling` resets attempts; staying in it (a cancel retry) does not.
func TestCompareAndSetEnteringCancellingResetsAttemptsOnlyOnEntry(t *testing.T) {
	pool := testdb.Connect(t)
	s := seed(t, pool, time.Hour, "confirmed")
	repo := NewOrders(pool)
	ctx := context.Background()
	if _, err := repo.Insert(ctx, newOrder(s, "T1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET attempts = 8 WHERE booking_id = $1`, s.booking); err != nil {
		t.Fatal(err)
	}
	o, _ := repo.GetByBookingID(ctx, s.booking)
	now := time.Now()
	o.Status, o.NextAttemptAt = domain.KitchenOrderCancelling, &now
	if ok, err := repo.CompareAndSet(ctx, o, domain.KitchenOrderSending, 8, now); err != nil || !ok {
		t.Fatalf("enter cancelling: ok=%v err=%v", ok, err)
	}
	got, _ := repo.GetByBookingID(ctx, s.booking)
	if got.Status != domain.KitchenOrderCancelling || got.Attempts != 0 {
		t.Fatalf("status=%s attempts=%d, want cancelling with 0", got.Status, got.Attempts)
	}
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET attempts = 3 WHERE booking_id = $1`, s.booking); err != nil {
		t.Fatal(err)
	}
	got.Attempts = 3
	if ok, err := repo.CompareAndSet(ctx, got, domain.KitchenOrderCancelling, 3, now); err != nil || !ok {
		t.Fatalf("cancel retry: ok=%v err=%v", ok, err)
	}
	again, _ := repo.GetByBookingID(ctx, s.booking)
	if again.Attempts != 3 {
		t.Errorf("attempts=%d after a cancel retry, want 3 (reset only on entry)", again.Attempts)
	}
}
