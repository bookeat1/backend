package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// paymentTables lists the tables these tests own, children first. restaurants,
// users and bookings are seeded with fresh UUIDs per test and deliberately
// NOT truncated, same convention as the booking package's own tests.
var paymentTables = []string{
	"payment_ledger_entries", "payment_outbox", "payment_events",
	"payment_refunds", "payments",
}

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, paymentTables...)
	return pool, context.Background()
}

func seedRestaurant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, id); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
		id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedBooking(t *testing.T, pool *pgxpool.Pool, restaurantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	starts := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at)
		 VALUES ($1,$2,'Гость','+7 777 123 45 67','+77771234567',2,$3,$4)`,
		id, restaurantID, starts, starts.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

// newPayment builds a valid, ready-to-insert domain.Payment for restaurantID
// / bookingID at status `created` — the caller moves it through the state
// machine with CompareAndSwapStatus / UpdateStatus as the test needs.
func newPayment(bookingID, restaurantID uuid.UUID) *domain.Payment {
	base := int64(500000)
	fee := int64(17500)
	return &domain.Payment{
		ID: uuid.New(), BookingID: bookingID, RestaurantID: restaurantID,
		Provider: domain.ProviderFreedomPay, Purpose: domain.PurposeDeposit,
		Status: domain.PaymentCreated, AmountMinor: base + fee, BaseAmountMinor: base,
		FeeMinor: fee, Currency: domain.CurrencyKZT,
		IdempotencyKey: bookingID.String() + ":" + uuid.New().String(),
	}
}
