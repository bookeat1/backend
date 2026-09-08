package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
	uc "backend-core/internal/usecase/analytics"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_outbox", "analytics_cursor")
	return pool, context.Background()
}

func seedBooking(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	restID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, restID); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	bookingID := uuid.New()
	starts := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at)
		 VALUES ($1,$2,'Гость','+7 777 123 45 67','+77771234567',2,$3,$4)`,
		bookingID, restID, starts, starts.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return bookingID
}

func insertOutbox(t *testing.T, pool *pgxpool.Pool, bookingID uuid.UUID, eventType string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO booking_outbox (id, booking_id, event_type, payload, created_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		id, bookingID, eventType, []byte(`{"id":"`+bookingID.String()+`","name":"PII"}`), at); err != nil {
		t.Fatalf("insert booking_outbox: %v", err)
	}
	return id
}

func TestCursorStore_RoundTrip(t *testing.T) {
	pool, ctx := setup(t)
	store := NewCursorStore(pool)

	// No row yet -> zero cursor.
	got, err := store.Get(ctx, uc.SourceBookingOutbox)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.CreatedAt.IsZero() || got.ID != uuid.Nil {
		t.Fatalf("empty cursor = %+v, want zero", got)
	}

	// Save then read back.
	want := uc.Cursor{CreatedAt: time.Now().UTC().Truncate(time.Microsecond), ID: uuid.New()}
	if err := store.Save(ctx, uc.SourceBookingOutbox, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = store.Get(ctx, uc.SourceBookingOutbox)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("round-trip cursor = %+v, want %+v", got, want)
	}

	// Save is an upsert (advances in place).
	next := uc.Cursor{CreatedAt: want.CreatedAt.Add(time.Second), ID: uuid.New()}
	if err := store.Save(ctx, uc.SourceBookingOutbox, next); err != nil {
		t.Fatalf("save3: %v", err)
	}
	got, _ = store.Get(ctx, uc.SourceBookingOutbox)
	if got.ID != next.ID {
		t.Fatalf("upsert did not advance: %+v", got)
	}
}

func TestSourceReader_ListSinceOrderedAndBounded(t *testing.T) {
	pool, ctx := setup(t)
	reader := NewSourceReader(pool)
	bookingID := seedBooking(t, pool)

	base := time.Now().UTC().Truncate(time.Microsecond)
	id1 := insertOutbox(t, pool, bookingID, "booking.created", base)
	id2 := insertOutbox(t, pool, bookingID, "booking.confirmed", base.Add(time.Second))
	id3 := insertOutbox(t, pool, bookingID, "booking.cancelled", base.Add(2*time.Second))

	// From the zero cursor: all three, ordered by (created_at, id).
	rows, err := reader.ListSince(ctx, uc.SourceBookingOutbox, uc.Cursor{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].ID != id1 || rows[1].ID != id2 || rows[2].ID != id3 {
		t.Fatalf("rows out of order: %v %v %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}

	// After the first row: only the two newer ones.
	rows, err = reader.ListSince(ctx, uc.SourceBookingOutbox, uc.Cursor{CreatedAt: rows[0].CreatedAt, ID: id1}, 100)
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != id2 {
		t.Fatalf("after-cursor rows wrong: %+v", rows)
	}

	// Limit is honoured.
	rows, err = reader.ListSince(ctx, uc.SourceBookingOutbox, uc.Cursor{}, 1)
	if err != nil {
		t.Fatalf("list3: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id1 {
		t.Fatalf("limit not honoured: %+v", rows)
	}
}
