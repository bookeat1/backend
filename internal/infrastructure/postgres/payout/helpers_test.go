package payout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// payoutTables lists the tables these tests own, children first. restaurants,
// users, bookings, payments and ledger entries are seeded with fresh UUIDs and
// NOT truncated, same convention as the payment package's own tests.
var payoutTables = []string{"payout_items", "payouts", "restaurant_payout_destinations"}

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, payoutTables...)
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

// seedPayment inserts a captured payment for the restaurant.
func seedPayment(t *testing.T, pool *pgxpool.Pool, restaurantID uuid.UUID) uuid.UUID {
	t.Helper()
	bookingID := seedBooking(t, pool, restaurantID)
	base, fee := int64(500000), int64(17500)
	p := &domain.Payment{
		ID: uuid.New(), BookingID: bookingID, RestaurantID: restaurantID,
		Provider: domain.ProviderFreedomPay, Purpose: domain.PurposeDeposit,
		Status: domain.PaymentCaptured, AmountMinor: base + fee, BaseAmountMinor: base,
		FeeMinor: fee, Currency: domain.CurrencyKZT,
		IdempotencyKey: bookingID.String() + ":" + uuid.New().String(),
	}
	if err := paymentrepo.New(pool).Create(context.Background(), p); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return p.ID
}

// seedLedgerEntry appends ONE restaurant-account ledger entry (balanced against
// a guest entry so the batch validates) and returns the restaurant entry id.
func seedLedgerEntry(t *testing.T, pool *pgxpool.Pool, paymentID uuid.UUID, dir domain.LedgerDirection, amountMinor int64) uuid.UUID {
	t.Helper()
	restEntry := domain.PaymentLedgerEntry{
		ID: uuid.New(), PaymentID: paymentID, Account: domain.AccountRestaurant,
		Direction: dir, AmountMinor: amountMinor, Currency: domain.CurrencyKZT,
		EntryType: domain.EntryCapture,
	}
	// Balancing guest entry on the opposite side.
	guestDir := domain.DirectionDebit
	if dir == domain.DirectionDebit {
		guestDir = domain.DirectionCredit
	}
	guestEntry := domain.PaymentLedgerEntry{
		ID: uuid.New(), PaymentID: paymentID, Account: domain.AccountGuest,
		Direction: guestDir, AmountMinor: amountMinor, Currency: domain.CurrencyKZT,
		EntryType: domain.EntryCapture,
	}
	if err := paymentrepo.NewLedger(pool).CreateBatch(context.Background(),
		[]domain.PaymentLedgerEntry{restEntry, guestEntry}); err != nil {
		t.Fatalf("seed ledger entry: %v", err)
	}
	return restEntry.ID
}

// newPayout builds a valid pending payout for a restaurant.
func newPayout(restaurantID uuid.UUID, amountMinor int64) *domain.Payout {
	id := uuid.New()
	return &domain.Payout{
		ID: id, RestaurantID: restaurantID, AmountMinor: amountMinor, Currency: domain.CurrencyKZT,
		Status: domain.PayoutPending, Method: domain.PayoutMethodFreedomPayCardToken,
		DestinationToken: uuid.NewString(), IdempotencyKey: "payout:" + id.String(),
	}
}

func txm(pool *pgxpool.Pool) *sqltx.Manager { return sqltx.NewManager(pool) }
