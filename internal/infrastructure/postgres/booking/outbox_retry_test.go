package booking

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// seedOutboxEvent commits one outbox row with an explicit creation time.
func seedOutboxEvent(t *testing.T, ctx context.Context, o *Outbox, bookingID uuid.UUID,
	typ domain.BookingEventType, createdAt time.Time) uuid.UUID {
	t.Helper()
	e := &domain.BookingOutboxEvent{BookingID: bookingID, EventType: typ, CreatedAt: createdAt}
	if err := o.Create(ctx, e); err != nil {
		t.Fatalf("seed outbox event: %v", err)
	}
	return e.ID
}

// The SQL half of the head-of-line-blocking fix (migration 0083). The
// dispatcher's unit tests drive an in-memory outbox; this one proves the real
// query behaves the same way: a rescheduled event disappears until it is due,
// never-attempted events are served before retries, and an abandoned event
// leaves the queue for good without being marked published.
func TestOutboxClaimDueFairness(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bookings, outbox := New(pool), NewOutbox(pool)
	txm := sqltx.NewManager(pool)

	b := newBooking(rid, time.Now().Add(24*time.Hour))
	if err := bookings.Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	// Two OLD events (the failing channel's) and one NEWER event.
	old1 := seedOutboxEvent(t, ctx, outbox, b.ID, domain.EventBookingCreated, now.Add(-2*time.Hour))
	old2 := seedOutboxEvent(t, ctx, outbox, b.ID, domain.EventBookingCreated, now.Add(-time.Hour))
	fresh := seedOutboxEvent(t, ctx, outbox, b.ID, domain.EventBookingCancelled, now.Add(-time.Minute))

	claim := func(limit int, at time.Time) []domain.BookingOutboxEvent {
		t.Helper()
		var got []domain.BookingOutboxEvent
		if err := txm.WithinTx(ctx, func(ctx context.Context) error {
			var err error
			got, err = outbox.ClaimDue(ctx, limit, at)
			return err
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		return got
	}

	// Batch of 2: the two oldest come first, as before the fix.
	first := claim(2, now)
	if len(first) != 2 || first[0].ID != old1 || first[1].ID != old2 {
		t.Fatalf("first claim = %+v, want the two oldest events", outboxIDs(first))
	}
	for _, e := range first {
		if e.Attempts != 0 || e.NextAttemptAt != nil || e.AbandonedAt != nil || e.LastError != "" {
			t.Fatalf("a never-attempted event carries retry state: %+v", e)
		}
	}

	// They fail. Reschedule pushes them past the batch AND into the future.
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return outbox.Reschedule(ctx, []domain.BookingOutboxFailure{
			{ID: old1, LastError: "whatsapp: 429", NextAttemptAt: now.Add(time.Minute)},
			{ID: old2, LastError: "whatsapp: 429", NextAttemptAt: now.Add(time.Minute)},
		})
	}); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	// THE POINT: with the batch still 2, the newer event is now claimable even
	// though the outage is still on. Before 0083 this returned old1/old2 again
	// and the fresh event was never reached.
	second := claim(2, now)
	if len(second) != 1 || second[0].ID != fresh {
		t.Fatalf("second claim = %+v, want only the fresh event", outboxIDs(second))
	}

	// Once due, the retries come back — with their bookkeeping intact — and a
	// never-attempted event still outranks them.
	newest := seedOutboxEvent(t, ctx, outbox, b.ID, domain.EventBookingCancelled, now)
	third := claim(10, now.Add(2*time.Minute))
	if len(third) != 4 {
		t.Fatalf("third claim = %+v, want all four events due", outboxIDs(third))
	}
	if third[0].ID != fresh && third[0].ID != newest {
		t.Fatalf("third claim starts with %s: a retry outranked a never-attempted event", third[0].ID)
	}
	if third[1].ID != fresh && third[1].ID != newest {
		t.Fatalf("third claim = %+v, want both never-attempted events before the retries", outboxIDs(third))
	}
	for _, e := range third {
		if e.ID != old1 && e.ID != old2 {
			continue
		}
		if e.Attempts != 1 || e.LastError != "whatsapp: 429" || e.NextAttemptAt == nil {
			t.Fatalf("rescheduled event lost its bookkeeping: %+v", e)
		}
	}

	// Out of budget: abandoning takes the event out of the queue for good and
	// leaves it UNPUBLISHED, so the dead letter stays distinguishable.
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return outbox.Abandon(ctx, []domain.BookingOutboxFailure{
			{ID: old1, LastError: "whatsapp: token expired"},
		}, now)
	}); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	for _, e := range claim(10, now.Add(24*time.Hour)) {
		if e.ID == old1 {
			t.Fatal("an abandoned event is still being claimed")
		}
	}
	var publishedAt, abandonedAt *time.Time
	var attempts int
	var lastErr *string
	if err := pool.QueryRow(ctx,
		`SELECT published_at, abandoned_at, attempts, last_error FROM booking_outbox WHERE id=$1`, old1).
		Scan(&publishedAt, &abandonedAt, &attempts, &lastErr); err != nil {
		t.Fatalf("read abandoned row: %v", err)
	}
	if publishedAt != nil {
		t.Fatal("abandoned event was marked published: giving up must not look like a delivery")
	}
	if abandonedAt == nil || attempts != 2 || lastErr == nil || *lastErr != "whatsapp: token expired" {
		t.Fatalf("dead letter row = abandoned_at:%v attempts:%d last_error:%v", abandonedAt, attempts, lastErr)
	}
}

func outboxIDs(evs []domain.BookingOutboxEvent) []uuid.UUID {
	out := make([]uuid.UUID, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}
