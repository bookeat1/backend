package admin

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/kwaakaorder"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/usecase/restaurants"
)

// dbRest reads the venue's linkage from the real restaurants row, so the test
// can re-link a venue with plain SQL.
type dbRest struct{ pool *pgxpool.Pool }

func (r dbRest) Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	var link *string
	if err := r.pool.QueryRow(ctx, `SELECT kwaaka_restaurant_id FROM restaurants WHERE id=$1`, id).Scan(&link); err != nil {
		return nil, domain.ErrNotFound
	}
	a := &domain.RestaurantAggregate{}
	a.ID, a.KwaakaRestaurantID = id, link
	return a, nil
}
func (dbRest) Update(context.Context, uuid.UUID, restaurants.SaveInput) (*domain.RestaurantAggregate, error) {
	return nil, fmt.Errorf("not used")
}

func newKwDBHarness(t *testing.T) (*UseCase, *pgxpool.Pool, uuid.UUID, Actor) {
	t.Helper()
	pool := testdb.Connect(t)
	ctx := context.Background()
	rid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO restaurants (id, name, city, price_category, kwaaka_restaurant_id, timezone)
		VALUES ($1,'R','Алматы','₸','kw-1','Asia/Almaty')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM restaurants WHERE id=$1`, rid) })
	pos := &kwPOS{tables: []domain.PosTable{{ID: "T1"}, {ID: "T2"}, {ID: "T3"}, {ID: "T4"}}}
	uc := NewUseCase(fakePerms{}, dbRest{pool}, &fakeMenu{}, &fakeWH{}, &fakeOverrides{}, &fakeGuests{},
		&fakeBookingList{}, &fakeBookingTx{}, &fakePaymentSettings{}, &fakeTelegramSettings{},
		WithKwaakaOrders(kwaakaorder.NewSettings(pool), pos, sqltx.NewManager(pool),
			KwaakaOrdersGlobal{Enabled: true, DefaultLead: time.Hour}))
	// updated_by has no FK (migration comment), a random uuid is fine.
	return uc, pool, rid, Actor{UserID: uuid.New(), Role: domain.RoleAdmin}
}

func TestKwaakaOrdersDB_LifecycleAndEnabledAt(t *testing.T) {
	uc, pool, rid, admin := newKwDBHarness(t)
	ctx := context.Background()

	v, err := uc.GetKwaakaOrders(ctx, admin, rid)
	if err != nil || v.Configured || v.WillSend {
		t.Fatalf("fresh venue: %+v err=%v", v, err)
	}

	on := KwaakaOrdersInput{OrdersEnabled: true, LeadMinutes: lead(45), Pool: []KwaakaPoolTableInput{{KwaakaTableID: "T2", Label: "Bar"}, {KwaakaTableID: "T1"}}}
	v, err = uc.SetKwaakaOrders(ctx, admin, rid, on)
	if err != nil || !v.WillSend {
		t.Fatalf("enable: %+v err=%v", v, err)
	}
	first := *v.EnabledAt

	// Read back through the repository the worker uses.
	got, err := kwaakaorder.NewSettings(pool).Get(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OrdersEnabled || got.KwaakaRestaurantID != "kw-1" || *got.LeadMinutes != 45 || got.UpdatedBy == nil || *got.UpdatedBy != admin.UserID {
		t.Fatalf("stored = %+v", got)
	}
	if len(got.Pool) != 2 || got.Pool[0].KwaakaTableID != "T2" || got.Pool[1].KwaakaTableID != "T1" {
		t.Fatalf("stored pool order = %+v", got.Pool)
	}

	time.Sleep(5 * time.Millisecond)
	on.LeadMinutes = nil // NULL = platform default
	v, err = uc.SetKwaakaOrders(ctx, admin, rid, on)
	if err != nil || !v.EnabledAt.Equal(first) || v.LeadMinutes != nil {
		t.Fatalf("still-on save moved enabled_at or kept lead: %+v err=%v", v, err)
	}

	off := on
	off.OrdersEnabled = false
	if v, err = uc.SetKwaakaOrders(ctx, admin, rid, off); err != nil || v.OrdersEnabled || !v.EnabledAt.Equal(first) {
		t.Fatalf("disable: %+v err=%v", v, err)
	}
	time.Sleep(5 * time.Millisecond)
	if v, err = uc.SetKwaakaOrders(ctx, admin, rid, on); err != nil || !v.EnabledAt.After(first) {
		t.Fatalf("re-enable must re-stamp enabled_at: %+v err=%v", v, err)
	}

	// Re-link the venue with SQL: the pool becomes stale, sending is blocked.
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET kwaaka_restaurant_id='kw-2' WHERE id=$1`, rid); err != nil {
		t.Fatal(err)
	}
	if v, err = uc.GetKwaakaOrders(ctx, admin, rid); err != nil || !v.PoolStale || v.WillSend {
		t.Fatalf("stale detection: %+v err=%v", v, err)
	}
	// A stale form (client still holds kw-1) is refused.
	on.KwaakaRestaurantID = "kw-1"
	if _, err = uc.SetKwaakaOrders(ctx, admin, rid, on); err == nil {
		t.Fatal("stale form accepted")
	}
	// Re-confirming against the new link re-snapshots and clears the staleness.
	on.KwaakaRestaurantID = "kw-2"
	if v, err = uc.SetKwaakaOrders(ctx, admin, rid, on); err != nil || v.PoolStale || !v.WillSend {
		t.Fatalf("re-confirm: %+v err=%v", v, err)
	}
}

func TestKwaakaOrdersDB_ConcurrentPutsLeaveOneWholePool(t *testing.T) {
	uc, pool, rid, admin := newKwDBHarness(t)
	ctx := context.Background()
	pools := [][]string{{"T1", "T2"}, {"T3"}, {"T4", "T3", "T1"}, {"T2"}}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		ids := pools[i%len(pools)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := uc.SetKwaakaOrders(ctx, admin, rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool0(ids...)})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PUT failed (PK clash or lock error?): %v", err)
		}
	}
	got, err := kwaakaorder.NewSettings(pool).Get(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got.Pool))
	for i, p := range got.Pool {
		ids[i] = p.KwaakaTableID
		if p.Position != i {
			t.Fatalf("positions not dense: %+v", got.Pool)
		}
	}
	for _, want := range pools {
		if fmt.Sprint(want) == fmt.Sprint(ids) {
			return
		}
	}
	t.Fatalf("final pool %v is a mix, not any one requested pool", ids)
}

func pool0(ids ...string) []KwaakaPoolTableInput { return pool(ids...) }

// PR #168 gate finding 1, on a real Postgres: a venue enabled with pool T1 and
// then unlinked (as the #149 endpoint does: kwaaka_restaurant_id NULL) must be
// switchable OFF, and re-linking to the same id must not resume sending.
func TestKwaakaOrdersDB_DisableWhileUnlinkedStaysOffAfterRelink(t *testing.T) {
	uc, pool, rid, admin := newKwDBHarness(t)
	ctx := context.Background()

	if v, err := uc.SetKwaakaOrders(ctx, admin, rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool0("T1")}); err != nil || !v.WillSend {
		t.Fatalf("enable: %+v err=%v", v, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET kwaaka_restaurant_id=NULL WHERE id=$1`, rid); err != nil {
		t.Fatal(err)
	}

	v, err := uc.SetKwaakaOrders(ctx, admin, rid, KwaakaOrdersInput{OrdersEnabled: false, Pool: pool0("T1")})
	if err != nil {
		t.Fatalf("PUT orders_enabled=false on an unlinked venue: %v", err)
	}
	if v.OrdersEnabled || v.WillSend {
		t.Fatalf("view after disable: %+v", v)
	}
	var enabled bool
	var snapshot string
	if err := pool.QueryRow(ctx, `SELECT orders_enabled, kwaaka_restaurant_id FROM restaurant_kwaaka_order_settings WHERE restaurant_id=$1`, rid).
		Scan(&enabled, &snapshot); err != nil {
		t.Fatal(err)
	}
	if enabled || snapshot != "kw-1" {
		t.Fatalf("db: orders_enabled=%v snapshot=%q, want false/kw-1", enabled, snapshot)
	}

	if _, err := pool.Exec(ctx, `UPDATE restaurants SET kwaaka_restaurant_id='kw-1' WHERE id=$1`, rid); err != nil {
		t.Fatal(err)
	}
	g, err := uc.GetKwaakaOrders(ctx, admin, rid)
	if err != nil || g.WillSend || g.OrdersEnabled {
		t.Fatalf("re-link to the same id resumed sending: %+v err=%v", g, err)
	}
}
