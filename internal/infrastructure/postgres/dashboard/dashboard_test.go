package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
)

// tables touched by the dashboard aggregates, truncated between tests.
// payment_providers is deliberately NOT truncated: migration 0007 seeds
// freedompay/tiptoppay and other integration tests (transport/rest/payments)
// rely on those rows surviving — wiping them here would break them under the
// shared TEST_DATABASE_URL.
var tables = []string{"payment_refunds", "payment_events", "payment_ledger_entries", "payments", "booking_tables", "bookings", "restaurants", "users"}

func seedRestaurant(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, name string, active bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, is_active) VALUES ($1,$2,'Алматы','₸',$3)`,
		id, name, active); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
}

func seedBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	start := createdAt.Add(24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status, created_at)
		 VALUES ($1,$2,'Guest','+7700','+7700',2,$3::timestamptz,$3::timestamptz + interval '2 hours',$4,$5)`,
		id, rid, start, status, createdAt); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

func seedProvider(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// payments.provider references payment_providers(provider); insert one enabled row.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO payment_providers (provider, is_enabled) VALUES ('freedompay', true) ON CONFLICT (provider) DO NOTHING`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
}

func seedCapturedPayment(t *testing.T, pool *pgxpool.Pool, rid, bid uuid.UUID, amountMinor int64, currency string, capturedAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO payments (id, booking_id, restaurant_id, provider, purpose, status, amount_minor, base_amount_minor, fee_minor, currency, idempotency_key, captured_at)
		 VALUES ($1,$2,$3,'freedompay','deposit','captured',$4,$4,0,$5,$6,$7)`,
		id, bid, rid, amountMinor, currency, id.String(), capturedAt); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return id
}

func seedRefund(t *testing.T, pool *pgxpool.Pool, paymentID uuid.UUID, amountMinor int64, currency, status string, createdAt time.Time) {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO payment_refunds (id, payment_id, amount_minor, currency, status, idempotency_key, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, paymentID, amountMinor, currency, status, id.String(), createdAt); err != nil {
		t.Fatalf("seed refund: %v", err)
	}
}

func TestOverviewCounters(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()

	r1, r2, r3 := uuid.New(), uuid.New(), uuid.New()
	seedRestaurant(t, pool, r1, "A", true)
	seedRestaurant(t, pool, r2, "B", true)
	seedRestaurant(t, pool, r3, "C", false) // inactive
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, phone) VALUES ($1,'+71'),($2,'+72')`, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	now := time.Now()
	seedBooking(t, pool, r1, "confirmed", now.Add(-2*24*time.Hour))  // in 7d + 30d
	seedBooking(t, pool, r1, "completed", now.Add(-10*24*time.Hour)) // in 30d only
	seedBooking(t, pool, r2, "cancelled", now.Add(-40*24*time.Hour)) // outside both

	o, err := New(pool).Overview(ctx)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if o.TotalRestaurants != 3 || o.ActiveRestaurants != 2 {
		t.Fatalf("restaurants: total=%d active=%d, want 3/2", o.TotalRestaurants, o.ActiveRestaurants)
	}
	if o.TotalUsers != 2 {
		t.Fatalf("users: got %d, want 2", o.TotalUsers)
	}
	if o.TotalBookings != 3 {
		t.Fatalf("total bookings: got %d, want 3", o.TotalBookings)
	}
	if o.BookingsLast7Days != 1 {
		t.Fatalf("bookings 7d: got %d, want 1", o.BookingsLast7Days)
	}
	if o.BookingsLast30Days != 2 {
		t.Fatalf("bookings 30d: got %d, want 2", o.BookingsLast30Days)
	}
}

func TestOverviewEmptyPlatformZeros(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	o, err := New(pool).Overview(context.Background())
	if err != nil {
		t.Fatalf("overview empty: %v", err)
	}
	if o != (structZero(o)) {
		t.Fatalf("empty platform overview must be all zeros, got %+v", o)
	}
}

// structZero returns the zero value of the same type for equality.
func structZero[T any](T) (z T) { return }

func TestBookingsByStatusNarrowsByPeriod(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	rid := uuid.New()
	seedRestaurant(t, pool, rid, "A", true)

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Inside window [base, base+10d):
	seedBooking(t, pool, rid, "confirmed", base.Add(1*24*time.Hour))
	seedBooking(t, pool, rid, "confirmed", base.Add(2*24*time.Hour))
	seedBooking(t, pool, rid, "cancelled", base.Add(3*24*time.Hour))
	seedBooking(t, pool, rid, "no_show", base.Add(4*24*time.Hour))
	// Outside window:
	seedBooking(t, pool, rid, "confirmed", base.Add(-1*24*time.Hour))
	seedBooking(t, pool, rid, "completed", base.Add(20*24*time.Hour))

	from := base
	to := base.Add(10 * 24 * time.Hour)
	counts, err := New(pool).BookingsByStatus(ctx, from, to)
	if err != nil {
		t.Fatalf("bookings by status: %v", err)
	}
	got := map[string]int64{}
	for _, c := range counts {
		got[string(c.Status)] = c.Count
	}
	if got["confirmed"] != 2 || got["cancelled"] != 1 || got["no_show"] != 1 {
		t.Fatalf("windowed counts wrong: %+v", got)
	}
	if _, ok := got["completed"]; ok {
		t.Fatalf("completed booking outside window must not appear: %+v", got)
	}
}

func TestPaymentsGMVSumsStoredAmounts(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	seedProvider(t, pool)
	rid := uuid.New()
	seedRestaurant(t, pool, rid, "A", true)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	from := base
	to := base.Add(10 * 24 * time.Hour)

	b1 := seedBooking(t, pool, rid, "completed", base)
	b2 := seedBooking(t, pool, rid, "completed", base)
	b3 := seedBooking(t, pool, rid, "completed", base)
	// Captured inside window, KZT: 500000 + 250000.
	p1 := seedCapturedPayment(t, pool, rid, b1, 500000, "KZT", base.Add(1*24*time.Hour))
	seedCapturedPayment(t, pool, rid, b2, 250000, "KZT", base.Add(2*24*time.Hour))
	// Captured outside window — must be excluded.
	seedCapturedPayment(t, pool, rid, b3, 999999, "KZT", base.Add(30*24*time.Hour))
	// A USD capture — must be excluded from a KZT query.
	b4 := seedBooking(t, pool, rid, "completed", base)
	seedCapturedPayment(t, pool, rid, b4, 111111, "USD", base.Add(1*24*time.Hour))

	// Refunds: one succeeded inside window (120000), one failed (ignored), one succeeded outside window.
	seedRefund(t, pool, p1, 120000, "KZT", "succeeded", base.Add(3*24*time.Hour))
	seedRefund(t, pool, p1, 50000, "KZT", "failed", base.Add(3*24*time.Hour))
	seedRefund(t, pool, p1, 70000, "KZT", "succeeded", base.Add(40*24*time.Hour))

	captured, refunded, err := New(pool).PaymentsGMV(ctx, from, to, "KZT")
	if err != nil {
		t.Fatalf("payments gmv: %v", err)
	}
	if captured.AmountMinor != 750000 || captured.Count != 2 {
		t.Fatalf("captured: got %d/%d, want 750000/2", captured.AmountMinor, captured.Count)
	}
	if refunded.AmountMinor != 120000 || refunded.Count != 1 {
		t.Fatalf("refunded: got %d/%d, want 120000/1", refunded.AmountMinor, refunded.Count)
	}

	// Empty-window query returns zeros, not an error.
	cEmpty, rEmpty, err := New(pool).PaymentsGMV(ctx, to, to.Add(24*time.Hour), "KZT")
	if err != nil {
		t.Fatalf("empty gmv: %v", err)
	}
	if cEmpty.AmountMinor != 0 || cEmpty.Count != 0 || rEmpty.AmountMinor != 0 || rEmpty.Count != 0 {
		t.Fatalf("empty window must be zeros, got captured=%+v refunded=%+v", cEmpty, rEmpty)
	}
}

func TestTopRestaurantsByBookingsAndGMV(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	seedProvider(t, pool)
	rA, rB, rC := uuid.New(), uuid.New(), uuid.New()
	seedRestaurant(t, pool, rA, "Alpha", true)
	seedRestaurant(t, pool, rB, "Beta", true)
	seedRestaurant(t, pool, rC, "Gamma", true)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	from := base
	to := base.Add(10 * 24 * time.Hour)

	// Bookings: B=3, A=2, C=1.
	for i := 0; i < 3; i++ {
		seedBooking(t, pool, rB, "confirmed", base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 2; i++ {
		seedBooking(t, pool, rA, "confirmed", base.Add(time.Duration(i)*time.Hour))
	}
	cBk := seedBooking(t, pool, rC, "completed", base)

	byBook, err := New(pool).TopRestaurantsByBookings(ctx, from, to, 2)
	if err != nil {
		t.Fatalf("top by bookings: %v", err)
	}
	if len(byBook) != 2 {
		t.Fatalf("limit not applied: got %d rows, want 2", len(byBook))
	}
	if byBook[0].RestaurantID != rB || byBook[0].BookingsCount != 3 {
		t.Fatalf("top1 by bookings: got %s/%d, want Beta/3", byBook[0].Name, byBook[0].BookingsCount)
	}
	if byBook[1].RestaurantID != rA || byBook[1].BookingsCount != 2 {
		t.Fatalf("top2 by bookings: got %s/%d, want Alpha/2", byBook[1].Name, byBook[1].BookingsCount)
	}

	// GMV: C=900000 (single big capture), A=100000. B has none.
	aBk := seedBooking(t, pool, rA, "completed", base)
	seedCapturedPayment(t, pool, rC, cBk, 900000, "KZT", base.Add(1*time.Hour))
	seedCapturedPayment(t, pool, rA, aBk, 100000, "KZT", base.Add(1*time.Hour))

	byGMV, err := New(pool).TopRestaurantsByGMV(ctx, from, to, "KZT", 10)
	if err != nil {
		t.Fatalf("top by gmv: %v", err)
	}
	if len(byGMV) != 2 {
		t.Fatalf("gmv rows: got %d, want 2 (only restaurants with captured money)", len(byGMV))
	}
	if byGMV[0].RestaurantID != rC || byGMV[0].GMVMinor != 900000 {
		t.Fatalf("top1 by gmv: got %s/%d, want Gamma/900000", byGMV[0].Name, byGMV[0].GMVMinor)
	}
	if byGMV[1].RestaurantID != rA || byGMV[1].GMVMinor != 100000 {
		t.Fatalf("top2 by gmv: got %s/%d, want Alpha/100000", byGMV[1].Name, byGMV[1].GMVMinor)
	}
}
