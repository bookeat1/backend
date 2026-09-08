package guest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestListByRestaurantAggregatesGuests(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "bookings", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	other := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸'),($2,'O','Алматы','₸')`,
		rid, other); err != nil {
		t.Fatalf("seed restaurants: %v", err)
	}

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Guest A: two bookings at rid (one completed), most recent name "Aiman v2".
	insertBooking(t, pool, rid, nil, "Aigul", "+7700", "completed", base)
	insertBooking(t, pool, rid, nil, "Aiman v2", "+7700", "pending", base.Add(48*time.Hour))
	// Guest B: one booking at rid, arrived, with an account.
	uid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, phone) VALUES ($1,'+7701')`, uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	insertBooking(t, pool, rid, &uid, "Bek", "+7701", "arrived", base.Add(24*time.Hour))
	// Noise: a booking at ANOTHER restaurant must never appear.
	insertBooking(t, pool, other, nil, "Ghost", "+7702", "completed", base)

	guests, err := New(pool).ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(guests) != 2 {
		t.Fatalf("distinct guests: got %d, want 2", len(guests))
	}
	byPhone := map[string]int{}
	for i, g := range guests {
		byPhone[g.PhoneNormalized] = i
	}
	a := guests[byPhone["+7700"]]
	if a.BookingsCount != 2 || a.VisitsCount != 1 {
		t.Errorf("guest A counts: bookings=%d visits=%d, want 2/1", a.BookingsCount, a.VisitsCount)
	}
	if a.Name != "Aiman v2" {
		t.Errorf("guest A name = %q, want most-recent 'Aiman v2'", a.Name)
	}
	if a.UserID != nil {
		t.Errorf("guest A user_id = %v, want nil (no account on any booking)", a.UserID)
	}
	b := guests[byPhone["+7701"]]
	if b.VisitsCount != 1 || b.UserID == nil || *b.UserID != uid {
		t.Errorf("guest B: visits=%d user_id=%v, want 1/%v", b.VisitsCount, b.UserID, uid)
	}
	// Ordering: most recent booking first → guest A (base+48h) precedes guest B (base+24h).
	if guests[0].PhoneNormalized != "+7700" {
		t.Errorf("ordering: first guest = %q, want +7700 (most recent booking)", guests[0].PhoneNormalized)
	}
}

func insertBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, uid *uuid.UUID, name, phone, status string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings
			(id, restaurant_id, user_id, name, phone, phone_normalized, guests, starts_at, ends_at, status, created_at)
		 VALUES ($1,$2,$3,$4,$5,$5,2,$6::timestamptz,$6::timestamptz + interval '2 hours',$7,$8)`,
		uuid.New(), rid, uid, name, phone, createdAt, status, createdAt); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
}
