package kwaakaorder

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// These tests EXECUTE every repository method against a real Postgres with
// realistic argument values. They exist because of a prod incident: ListPollDue
// shipped with `sent_at > $1 - interval '12 hours'`, where Postgres infers $1 as
// an interval and the statement can never run (SQLSTATE 42883). Every other test
// either used fakes or never reached that method, so the worker logged the error
// every 30 s on prod. A statement that cannot be planned must fail here.
//
// Assertions are deliberately semantic (the right row comes back), not just
// "no error": a query that runs and returns nothing is the next-cheapest bug.

type fullSeed struct {
	seeded
	item    uuid.UUID
	payment uuid.UUID
}

// seedFull creates a venue with settings + pool, a confirmed booking starting in
// startsIn with one item (linked to a Kwaaka-synced menu item) and a captured
// preorder payment.
func seedFull(t *testing.T, pool *pgxpool.Pool, startsIn time.Duration) fullSeed {
	t.Helper()
	ctx := context.Background()
	s := fullSeed{seeded: seed(t, pool, startsIn, "confirmed"), item: uuid.New(), payment: uuid.New()}
	menu := uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %.40s: %v", q, err)
		}
	}
	exec(`INSERT INTO menu_items (id, restaurant_id, name, price, kwaaka_product_id) VALUES ($1,$2,'Плов',2500,'kw-prod-1')`, menu, s.restaurant)
	exec(`INSERT INTO booking_items (id, booking_id, menu_item_id, item_name, item_price_minor, quantity, status, comment)
	      VALUES ($1,$2,$3,'Плов',250000,2,'pending','без лука')`, s.item, s.booking, menu)
	exec(`INSERT INTO payments (id, booking_id, restaurant_id, provider, purpose, status, amount_minor, base_amount_minor,
	        idempotency_key, captured_at) VALUES ($1,$2,$3,'tiptoppay','preorder','captured',500000,500000,$4, now())`,
		s.payment, s.booking, s.restaurant, uuid.NewString())
	set := &domain.KwaakaOrderSettings{RestaurantID: s.restaurant, KwaakaRestaurantID: "kw-1", OrdersEnabled: true,
		Pool: []domain.KwaakaPoolTable{{KwaakaTableID: "T1", Position: 1, Label: "Стол 1"}, {KwaakaTableID: "T2", Position: 2, Label: "Стол 2"}}}
	if err := NewSettings(pool).Save(ctx, set); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	// An enabled venue with a due booking is a candidate for ANY worker Tick that
	// runs later against this shared database (other packages' tests do): leave
	// nothing behind.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurant_kwaaka_order_settings WHERE restaurant_id = $1`, s.restaurant)
	})
	return s
}

func hasID(rows []domain.KitchenOrder, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func TestOrdersEveryMethodExecutes(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := NewOrders(pool)
	testdb.Truncate(t, pool, "kwaaka_kitchen_orders")
	now := time.Now()

	s := seedFull(t, pool, time.Hour)

	t.Run("ListCandidates", func(t *testing.T) {
		got, err := repo.ListCandidates(ctx, now, 200)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range got {
			if c.BookingID == s.booking && c.RestaurantID == s.restaurant && c.KwaakaRestaurantID == "kw-1" {
				found = true
			}
		}
		if !found {
			t.Fatalf("candidate not returned: %+v", got)
		}
	})

	t.Run("claim subject, locked and unlocked", func(t *testing.T) {
		got, err := repo.GetClaimSubject(ctx, s.booking)
		if err != nil {
			t.Fatal(err)
		}
		if got.KwaakaID != "kw-1" || got.Timezone != "Asia/Almaty" || !got.Paid || got.PaidMinor != 500000 ||
			got.CapturedAt == nil || len(got.Items) != 1 || got.Items[0].KwaakaProductID == nil || *got.Items[0].KwaakaProductID != "kw-prod-1" {
			t.Fatalf("subject: %+v", got)
		}
		// FOR UPDATE needs a transaction to be meaningful, but the statement must run either way.
		if _, err := repo.LockClaimSubject(ctx, s.booking); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.GetClaimSubject(ctx, uuid.New()); err == nil {
			t.Fatal("unknown booking must be ErrNotFound")
		}
	})

	o := newOrder(s.seeded, "T1")
	t.Run("Insert and getters", func(t *testing.T) {
		if ok, err := repo.Insert(ctx, o); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
		if got, err := repo.GetByID(ctx, o.ID); err != nil || got.BookingID != s.booking {
			t.Fatalf("GetByID: %+v %v", got, err)
		}
		if got, err := repo.GetByBookingID(ctx, s.booking); err != nil || got.ID != o.ID {
			t.Fatalf("GetByBookingID: %+v %v", got, err)
		}
		if _, err := repo.GetByPosOrderID(ctx, "no-such"); err == nil {
			t.Fatal("unknown pos id must be ErrNotFound")
		}
	})

	t.Run("ListCandidates skips a booking that already has an order", func(t *testing.T) {
		got, err := repo.ListCandidates(ctx, now, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.BookingID == s.booking {
				t.Fatal("booking with an order must not be a candidate")
			}
		}
	})

	t.Run("ListByBookingIDs", func(t *testing.T) {
		got, err := repo.ListByBookingIDs(ctx, []uuid.UUID{s.booking, uuid.New()})
		if err != nil {
			t.Fatal(err)
		}
		v, ok := got[s.booking]
		if !ok || len(got) != 1 || v.Status != domain.KitchenOrderSending || v.TableLabel != "Стол 1" {
			t.Fatalf("views: %+v", got)
		}
		if m, err := repo.ListByBookingIDs(ctx, nil); err != nil || len(m) != 0 {
			t.Fatalf("empty ids: %v %v", m, err)
		}
	})

	t.Run("LeaseDue", func(t *testing.T) {
		rows, err := repo.LeaseDue(ctx, now, 2*time.Minute, 20)
		if err != nil {
			t.Fatal(err)
		}
		if !hasID(rows, o.ID) {
			t.Fatalf("due row not leased: %+v", rows)
		}
	})

	// CompareAndSet: sending -> sent with every nullable argument NULL, then with all set.
	t.Run("CompareAndSet sending to sent, nullable args NULL", func(t *testing.T) {
		cur, _ := repo.GetByID(ctx, o.ID)
		want, attempts := cur.Status, cur.Attempts
		cur.Status, cur.NextAttemptAt = domain.KitchenOrderSent, nil
		sent := time.Now()
		cur.SentAt = &sent
		posID := "pos-1"
		cur.KwaakaOrderID = &posID
		if ok, err := repo.CompareAndSet(ctx, cur, want, attempts, now); err != nil || !ok {
			t.Fatalf("cas: %v %v", ok, err)
		}
		got, _ := repo.GetByID(ctx, o.ID)
		if got.Status != domain.KitchenOrderSent || got.NextAttemptAt != nil || got.KwaakaOrderID == nil {
			t.Fatalf("after cas: %+v", got)
		}
		if got, err := repo.GetByPosOrderID(ctx, "pos-1"); err != nil || got.ID != o.ID {
			t.Fatalf("GetByPosOrderID: %+v %v", got, err)
		}
	})

	t.Run("CompareAndSet with every field populated", func(t *testing.T) {
		cur, _ := repo.GetByID(ctx, o.ID)
		want, attempts := cur.Status, cur.Attempts
		st, raw, src, errText, code, reason := time.Now(), "Printed", "poll", "boom", "E1", "booking_cancelled"
		cur.PosStatusRaw, cur.PosStatusAt, cur.PosStatusSource = &raw, &st, &src
		ps := domain.PosStateBillPrinted
		cur.PosState = &ps
		cur.LastError, cur.ErrorCode, cur.CancelReason = &errText, &code, &reason
		cur.CancelRequestedAt, cur.TableReleasedAt = &st, &st
		cur.OutcomeUnknown, cur.Partial = true, true
		cur.BookingStartsAt = st.Add(time.Hour)
		next := st.Add(time.Minute)
		cur.NextAttemptAt = &next
		if ok, err := repo.CompareAndSet(ctx, cur, want, attempts, now); err != nil || !ok {
			t.Fatalf("cas: %v %v", ok, err)
		}
		got, _ := repo.GetByID(ctx, o.ID)
		if got.PosState == nil || *got.PosState != domain.PosStateBillPrinted || got.CancelRequestedAt == nil || got.TableReleasedAt == nil {
			t.Fatalf("after cas: %+v", got)
		}
	})

	t.Run("CompareAndSet sent over a stored cancel flag with NULL next attempt uses the worker clock", func(t *testing.T) {
		s2 := seedFull(t, pool, time.Hour)
		o2 := newOrder(s2.seeded, "T2")
		if _, err := repo.Insert(ctx, o2); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET cancel_requested_at = now() WHERE id = $1`, o2.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ := repo.GetByID(ctx, o2.ID)
		cur.CancelRequestedAt = nil // the caller's copy predates the flag
		cur.Status, cur.NextAttemptAt = domain.KitchenOrderSent, nil
		workerNow := time.Now().Add(7 * time.Minute)
		if ok, err := repo.CompareAndSet(ctx, cur, domain.KitchenOrderSending, 0, workerNow); err != nil || !ok {
			t.Fatalf("cas: %v %v", ok, err)
		}
		got, _ := repo.GetByID(ctx, o2.ID)
		if got.Status != domain.KitchenOrderCancelling || got.NextAttemptAt == nil || got.NextAttemptAt.Sub(workerNow).Abs() > time.Second {
			t.Fatalf("want cancelling due at worker clock, got %+v", got)
		}
	})

	// TableLoads: o is `sent`, bill_printed, not released, hold in the future.
	t.Run("TableLoads", func(t *testing.T) {
		s3 := seedFull(t, pool, time.Hour)
		o3 := newOrder(s3.seeded, "T2")
		o3.Status = domain.KitchenOrderSent
		sentAt := time.Now()
		o3.SentAt, o3.NextAttemptAt = &sentAt, nil
		if _, err := repo.Insert(ctx, o3); err != nil {
			t.Fatal(err)
		}
		// Insert does not store sent_at: set it the way the worker does.
		if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET sent_at = $2 WHERE id = $1`, o3.ID, sentAt); err != nil {
			t.Fatal(err)
		}
		load, err := repo.TableLoads(ctx, s3.restaurant, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if load.Live["T2"] != 1 || load.Inflight["T2"] != 1 {
			t.Fatalf("fresh sent order: inflight=%v live=%v", load.Inflight, load.Live)
		}
		// Sent long ago: still live, no longer in flight.
		if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET sent_at = $2 WHERE id = $1`, o3.ID, sentAt.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		load, err = repo.TableLoads(ctx, s3.restaurant, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if load.Live["T2"] != 1 || load.Inflight["T2"] != 0 {
			t.Fatalf("old sent order: inflight=%v live=%v", load.Inflight, load.Live)
		}
	})

	t.Run("ListCancelledPending", func(t *testing.T) {
		s4 := seedFull(t, pool, time.Hour)
		o4 := newOrder(s4.seeded, "T1")
		if _, err := repo.Insert(ctx, o4); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE bookings SET status = 'cancelled' WHERE id = $1`, s4.booking); err != nil {
			t.Fatal(err)
		}
		rows, err := repo.ListCancelledPending(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !hasID(rows, o4.ID) {
			t.Fatalf("cancelled booking's order not returned: %+v", rows)
		}
	})

	t.Run("ListRescheduled", func(t *testing.T) {
		s5 := seedFull(t, pool, time.Hour)
		o5 := newOrder(s5.seeded, "T1")
		o5.Status = domain.KitchenOrderSent
		o5.NextAttemptAt = nil
		o5.BookingStartsAt = time.Now().Add(-48 * time.Hour) // differs from the booking's starts_at
		if _, err := repo.Insert(ctx, o5); err != nil {
			t.Fatal(err)
		}
		rows, err := repo.ListRescheduled(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			if r.Order.ID == o5.ID && !r.NewStartsAt.IsZero() {
				found = true
			}
		}
		if !found {
			t.Fatalf("rescheduled order not returned: %+v", rows)
		}
	})

	// The incident: ListPollDue with real time arguments.
	t.Run("ListPollDue", func(t *testing.T) {
		s6 := seedFull(t, pool, time.Hour)
		fresh := newOrder(s6.seeded, "T1") // sent 1 minute ago, never polled
		fresh.Status, fresh.NextAttemptAt = domain.KitchenOrderSent, nil
		if _, err := repo.Insert(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		s7 := seedFull(t, pool, time.Hour)
		old := newOrder(s7.seeded, "T1") // sent 13 h ago: outside the 12 h poll horizon
		old.Status, old.NextAttemptAt = domain.KitchenOrderSent, nil
		if _, err := repo.Insert(ctx, old); err != nil {
			t.Fatal(err)
		}
		s8 := seedFull(t, pool, time.Hour)
		closed := newOrder(s8.seeded, "T1") // closed in the POS: never polled again
		closed.Status, closed.NextAttemptAt = domain.KitchenOrderSent, nil
		if _, err := repo.Insert(ctx, closed); err != nil {
			t.Fatal(err)
		}
		set := func(id uuid.UUID, sentAgo time.Duration, posState *string) {
			if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET sent_at = $2, pos_state = $3 WHERE id = $1`,
				id, time.Now().Add(-sentAgo), posState); err != nil {
				t.Fatal(err)
			}
		}
		closedState := "closed"
		set(fresh.ID, time.Minute, nil)
		set(old.ID, 13*time.Hour, nil)
		set(closed.ID, time.Minute, &closedState)

		rows, err := repo.ListPollDue(ctx, time.Now(), time.Now().Add(-5*time.Minute), 50)
		if err != nil {
			t.Fatal(err)
		}
		if !hasID(rows, fresh.ID) {
			t.Fatalf("fresh sent order must be poll-due: %+v", rows)
		}
		if hasID(rows, old.ID) {
			t.Fatal("order sent 13h ago is outside the poll horizon")
		}
		if hasID(rows, closed.ID) {
			t.Fatal("closed order must not be polled")
		}
		// Polled a moment ago: not stale yet.
		if _, err := pool.Exec(ctx, `UPDATE kwaaka_kitchen_orders SET pos_status_at = now() WHERE id = $1`, fresh.ID); err != nil {
			t.Fatal(err)
		}
		rows, err = repo.ListPollDue(ctx, time.Now(), time.Now().Add(-5*time.Minute), 50)
		if err != nil {
			t.Fatal(err)
		}
		if hasID(rows, fresh.ID) {
			t.Fatal("recently polled order must wait for the next interval")
		}
	})
}

func TestSettingsEveryMethodExecutes(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	s := seedFull(t, pool, time.Hour)
	repo := NewSettings(pool)

	lead := 45
	got, err := repo.Get(ctx, s.restaurant)
	if err != nil || !got.OrdersEnabled || len(got.Pool) != 2 || got.Pool[0].KwaakaTableID != "T1" {
		t.Fatalf("Get: %+v %v", got, err)
	}
	got.LeadMinutes = &lead
	if err := repo.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	locked, err := repo.LockForUpdate(ctx, s.restaurant)
	if err != nil || locked.LeadMinutes == nil || *locked.LeadMinutes != 45 {
		t.Fatalf("LockForUpdate: %+v %v", locked, err)
	}
	if _, err := repo.Get(ctx, uuid.New()); err == nil {
		t.Fatal("unknown restaurant must be ErrNotFound")
	}
}

func TestWebhooksEveryMethodExecutes(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "kwaaka_webhook_events")
	repo := NewWebhooks(pool)
	now := time.Now()

	a := &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookOrderStatus, DedupKey: uuid.NewString(), Body: []byte(`{"id":"a"}`)}
	b := &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookOrderStatus, DedupKey: uuid.NewString(), Body: []byte(`{"id":"b"}`)}
	c := &domain.KwaakaWebhookEvent{Kind: domain.KwaakaWebhookReserveStatus, DedupKey: uuid.NewString(), Body: []byte(`{"id":"c"}`)}
	for _, e := range []*domain.KwaakaWebhookEvent{a, b, c} {
		if ok, err := repo.Insert(ctx, e); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
	}

	rows, err := repo.LockDue(ctx, now, 20)
	if err != nil || len(rows) != 3 {
		t.Fatalf("LockDue: %d rows, %v", len(rows), err)
	}

	// Defer one event into the future: it must drop out of LockDue (NULL and non-NULL next_attempt_at).
	if err := repo.Defer(ctx, b.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = repo.LockDue(ctx, now, 20)
	if err != nil || len(rows) != 2 {
		t.Fatalf("LockDue after Defer: %d rows, %v", len(rows), err)
	}
	rows, err = repo.LockDue(ctx, now.Add(2*time.Hour), 20)
	if err != nil || len(rows) != 3 {
		t.Fatalf("LockDue later: %d rows, %v", len(rows), err)
	}

	// Finish with and without the optional arguments.
	if err := repo.Finish(ctx, a.ID, domain.WebhookOutcomeStored, nil, nil); err != nil {
		t.Fatal(err)
	}
	msg := "unparseable"
	s := seedFull(t, pool, time.Hour)
	ord := newOrder(s.seeded, "T1")
	if _, err := NewOrders(pool).Insert(ctx, ord); err != nil {
		t.Fatal(err)
	}
	if err := repo.Finish(ctx, c.ID, domain.WebhookOutcomeUnparseable, &msg, &ord.ID); err != nil {
		t.Fatal(err)
	}

	// Prune: only processed events older than the cutoff go.
	if _, err := pool.Exec(ctx, `UPDATE kwaaka_webhook_events SET processed_at = $2 WHERE id = $1`, a.ID, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := repo.PruneProcessed(ctx, now.Add(-30*24*time.Hour), 500)
	if err != nil || n != 1 {
		t.Fatalf("PruneProcessed: %d %v (want exactly the 40-day-old processed event)", n, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kwaaka_webhook_events`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("left=%d err=%v", left, err)
	}
}
