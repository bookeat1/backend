package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
)

// seedEventTicket inserts a restaurant → event → event_ticket chain and returns
// the ticket id, so a polymorphic ticket payment has a real FK parent.
func seedEventTicket(t *testing.T, pool *pgxpool.Pool, restaurantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.New()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, ticketed, ticket_price_minor, capacity)
		 VALUES ($1,$2,'E',$3,$4,'published',true,35000,100)`,
		eventID, restaurantID, start, start.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	ticketID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_tickets (id, event_id, restaurant_id, quantity, unit_price_minor, total_minor, status, purchase_idempotency_key)
		 VALUES ($1,$2,$3,2,35000,70000,'pending',$4)`,
		ticketID, eventID, restaurantID, uuid.NewString()); err != nil {
		t.Fatalf("seed event ticket: %v", err)
	}
	return ticketID
}

// TestTicketPaymentRoundTrip proves a POLYMORPHIC ticket payment (booking_id
// NULL, event_ticket_id set) inserts against the chk_payments_subject XOR check
// and reads back with the subject intact — the money-path half of migration
// 0045.
func TestTicketPaymentRoundTrip(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)
	ticketID := seedEventTicket(t, pool, rid)

	base := int64(70000)
	fee := int64(2538)
	p := &domain.Payment{
		ID: uuid.New(), EventTicketID: &ticketID, RestaurantID: rid,
		Provider: domain.ProviderFreedomPay, Purpose: domain.PurposeTicket,
		Status: domain.PaymentCreated, AmountMinor: base + fee, BaseAmountMinor: base, FeeMinor: fee,
		Currency: domain.CurrencyKZT, IdempotencyKey: "ticket:" + ticketID.String(),
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create ticket payment: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BookingID != uuid.Nil {
		t.Fatalf("booking_id should be Nil for a ticket payment, got %s", got.BookingID)
	}
	if got.EventTicketID == nil || *got.EventTicketID != ticketID {
		t.Fatalf("event_ticket_id not preserved: %v", got.EventTicketID)
	}
	if got.Purpose != domain.PurposeTicket || got.AmountMinor != base+fee {
		t.Fatalf("ticket payment fields wrong: purpose=%s amount=%d", got.Purpose, got.AmountMinor)
	}

	// A ticket payment must NOT count as a live payment for booking uuid.Nil
	// (booking_id is NULL, excluded from idx_payments_live_per_booking).
	if _, err := repo.GetLiveByBookingID(ctx, uuid.Nil); err == nil {
		t.Fatalf("a ticket payment must not resolve as a live booking payment for Nil")
	}

	// Move to captured and prove the captured→refunded CAS works for it.
	now := time.Now()
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, now); err != nil {
		t.Fatalf("cas authorized: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, now); err != nil {
		t.Fatalf("cas captured: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, domain.PaymentRefunded, now); err != nil {
		t.Fatalf("cas refunded: %v", err)
	}
}

// TestBookingPaymentStillRoundTrips guards that the polymorphic column did not
// break an ordinary booking payment (booking_id set, event_ticket_id NULL).
func TestBookingPaymentStillRoundTrips(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create booking payment: %v", err)
	}
	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BookingID != bid {
		t.Fatalf("booking_id lost: %s want %s", got.BookingID, bid)
	}
	if got.EventTicketID != nil {
		t.Fatalf("event_ticket_id should be nil for a booking payment, got %v", got.EventTicketID)
	}
}
