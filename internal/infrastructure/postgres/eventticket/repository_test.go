package eventticket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// seed creates a restaurant + a ticketed event with the given capacity and
// returns (eventID, restaurantID).
func seed(ctx context.Context, t *testing.T, pool sqltx.Querier, capacity int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	rrepo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: "R", City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := rrepo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	erepo := eventrepo.New(pool)
	price := int64(35000)
	cap := capacity
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	e := &domain.Event{
		ID: uuid.New(), RestaurantID: r.ID, Title: "E", StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.EventPublished, Ticketed: true, TicketPriceMinor: &price, Capacity: &cap,
	}
	if err := erepo.Create(ctx, e); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return e.ID, r.ID
}

func mkTicket(eventID, restaurantID uuid.UUID, qty int, key string) *domain.EventTicket {
	return &domain.EventTicket{
		ID: uuid.New(), EventID: eventID, RestaurantID: restaurantID, Quantity: qty,
		UnitPriceMinor: 35000, TotalMinor: int64(qty) * 35000, Currency: domain.CurrencyKZT,
		Status: domain.TicketPending, PurchaseIdempotencyKey: key,
		// Purchase-time snapshot of the event's refund policy — see
		// domain.EventTicket.RefundPolicy.
		RefundPolicyRefundable: true, RefundPolicyCutoffMinutes: 120,
	}
}

// reserve replicates the purchase usecase's race-safe reservation exactly:
// lock the event, read the sold count, reject an oversell, insert the pending
// ticket — all in ONE transaction.
func reserve(ctx context.Context, repo *Repository, txm *sqltx.Manager, eventID, restaurantID uuid.UUID, qty, capacity int, key string) error {
	return txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := repo.LockEventForCapacity(ctx, eventID); err != nil {
			return err
		}
		sold, err := repo.SoldCount(ctx, eventID)
		if err != nil {
			return err
		}
		if sold+qty > capacity {
			return domain.ErrAlreadyExists // sold out
		}
		return repo.Create(ctx, mkTicket(eventID, restaurantID, qty, key))
	})
}

// TestConcurrentLastSeat is the money-critical test: two buyers race for the
// last seat; the FOR UPDATE lock + capacity check must let exactly ONE win.
func TestConcurrentLastSeat(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "event_tickets", "events", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	txm := sqltx.NewManager(pool)

	// Capacity 1, two buyers each wanting the single remaining seat.
	eventID, rid := seed(ctx, t, pool, 1)

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = reserve(ctx, repo, txm, eventID, rid, 1, 1, uuid.NewString())
		}(i)
	}
	close(start)
	wg.Wait()

	wins, losses := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, domain.ErrAlreadyExists):
			losses++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("last-seat race: wins=%d losses=%d, want exactly one winner", wins, losses)
	}
	// The DB must hold exactly one holding ticket — no oversell.
	if sold, _ := repo.SoldCount(ctx, eventID); sold != 1 {
		t.Fatalf("oversold: sold = %d, want 1", sold)
	}
}

// TestCapacityCountsAndReleaseFreesSeats: sold count reflects pending+paid;
// cancelling/refunding frees the seat.
func TestCapacityCountsAndReleaseFreesSeats(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "event_tickets", "events", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	eventID, rid := seed(ctx, t, pool, 10)

	pend := mkTicket(eventID, rid, 3, "k-pend")
	paid := mkTicket(eventID, rid, 2, "k-paid")
	for _, tk := range []*domain.EventTicket{pend, paid} {
		if err := repo.Create(ctx, tk); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := repo.CompareAndSwapStatus(ctx, paid.ID, domain.TicketPending, domain.TicketPaid, time.Now()); err != nil {
		t.Fatalf("cas paid: %v", err)
	}
	if sold, _ := repo.SoldCount(ctx, eventID); sold != 5 {
		t.Fatalf("sold = %d, want 5 (3 pending + 2 paid)", sold)
	}

	// Cancel the pending → frees 3.
	if err := repo.CompareAndSwapStatus(ctx, pend.ID, domain.TicketPending, domain.TicketCancelled, time.Now()); err != nil {
		t.Fatalf("cas cancel: %v", err)
	}
	if sold, _ := repo.SoldCount(ctx, eventID); sold != 2 {
		t.Fatalf("after cancel sold = %d, want 2", sold)
	}
	// Refund the paid → frees the remaining 2.
	if err := repo.CompareAndSwapStatus(ctx, paid.ID, domain.TicketPaid, domain.TicketRefunded, time.Now()); err != nil {
		t.Fatalf("cas refund: %v", err)
	}
	if sold, _ := repo.SoldCount(ctx, eventID); sold != 0 {
		t.Fatalf("after refund sold = %d, want 0", sold)
	}

	counts, err := repo.Counts(ctx, eventID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.CancelledTickets != 1 || counts.RefundedTickets != 1 || counts.PaidTickets != 0 {
		t.Fatalf("counts wrong: %+v", counts)
	}
}

// TestCASStatusIdempotent: a second CAS from a stale `from` loses (ErrAlreadyExists),
// so a duplicate projection moves the ticket at most once.
func TestCASStatusIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "event_tickets", "events", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	eventID, rid := seed(ctx, t, pool, 10)
	tk := mkTicket(eventID, rid, 1, "k")
	if err := repo.Create(ctx, tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The refund-policy snapshot must survive the round-trip: the terms are only
	// keepable if the row remembers the rules it was sold under.
	stored, err := repo.GetByID(ctx, tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.RefundPolicy() != (domain.TicketRefundPolicy{Refundable: true, CutoffMinutes: 120}) {
		t.Fatalf("refund policy snapshot not persisted: %+v", stored.RefundPolicy())
	}
	if err := repo.CompareAndSwapStatus(ctx, tk.ID, domain.TicketPending, domain.TicketPaid, time.Now()); err != nil {
		t.Fatalf("first cas: %v", err)
	}
	// Redelivery: pending→paid again must lose (already paid).
	if err := repo.CompareAndSwapStatus(ctx, tk.ID, domain.TicketPending, domain.TicketPaid, time.Now()); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second cas err = %v, want ErrAlreadyExists", err)
	}
	// Unknown id → ErrNotFound.
	if err := repo.CompareAndSwapStatus(ctx, uuid.New(), domain.TicketPending, domain.TicketPaid, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing id err = %v, want ErrNotFound", err)
	}
}

// TestIdempotencyUnique: two tickets with the same (event, key) collide.
func TestIdempotencyUnique(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "event_tickets", "events", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	eventID, rid := seed(ctx, t, pool, 10)
	if err := repo.Create(ctx, mkTicket(eventID, rid, 1, "dup")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := repo.Create(ctx, mkTicket(eventID, rid, 1, "dup")); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("dup key err = %v, want ErrAlreadyExists", err)
	}
}
