package booking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// seedCapacityVenue creates a venue that books by declared capacity instead of
// tables (migration 0054).
func seedCapacityVenue(t *testing.T, pool *pgxpool.Pool, seats int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, booking_capacity_mode, booking_capacity_seats)
		 VALUES ($1,'Capacity R','Алматы','₸','seats',$2)`, id, seats); err != nil {
		t.Fatalf("seed capacity restaurant: %v", err)
	}
	return id
}

// holdsFor builds one booking's holds over `buckets` consecutive 15-minute
// buckets starting at `from`, mirroring what buildCapacityHolds produces in the
// usecase layer.
func holdsFor(bookingID, rid uuid.UUID, from time.Time, buckets, seats, limit int) []domain.BookingCapacityHold {
	out := make([]domain.BookingCapacityHold, 0, buckets)
	for i := 0; i < buckets; i++ {
		out = append(out, domain.BookingCapacityHold{
			BookingID: bookingID, RestaurantID: rid,
			BucketStart: from.Add(time.Duration(i) * domain.CapacityBucket),
			Seats:       seats, SeatsLimit: limit,
		})
	}
	return out
}

func seedCapacityBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, start time.Time, guests int) *domain.Booking {
	t.Helper()
	b := newBooking(rid, start)
	b.Guests = guests
	if err := New(pool).Create(context.Background(), b); err != nil {
		t.Fatalf("create booking: %v", err)
	}
	return b
}

// TestCapacityHoldsExactBoundary pins the boundary the whole feature turns on:
// the declared capacity may be filled to the last seat, and the very next seat
// is refused — by the database, as a conflict rather than a 500.
func TestCapacityHoldsExactBoundary(t *testing.T) {
	pool, ctx := setup(t)
	const seats = 10
	rid := seedCapacityVenue(t, pool, seats)
	capacity := NewCapacity(pool)
	start := time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC)

	// 6 + 4 = exactly 10 in the same buckets.
	first := seedCapacityBooking(t, pool, rid, start, 6)
	if err := capacity.Create(ctx, holdsFor(first.ID, rid, start, 8, 6, seats)); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	second := seedCapacityBooking(t, pool, rid, start, 4)
	if err := capacity.Create(ctx, holdsFor(second.ID, rid, start, 8, 4, seats)); err != nil {
		t.Fatalf("hold filling the venue to the last seat must be accepted: %v", err)
	}

	// One more guest in an OVERLAPPING window — only the first bucket collides,
	// which must be enough to refuse the whole booking.
	third := seedCapacityBooking(t, pool, rid, start, 1)
	err := capacity.Create(ctx, holdsFor(third.ID, rid, start.Add(7*domain.CapacityBucket), 8, 1, seats))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("11th seat = %v, want ErrAlreadyExists", err)
	}

	// And the bucket counter is exactly the capacity, not more.
	var taken int
	if err := pool.QueryRow(ctx,
		`SELECT seats_taken FROM restaurant_capacity_buckets WHERE restaurant_id=$1 AND bucket_start=$2`,
		rid, start).Scan(&taken); err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if taken != seats {
		t.Fatalf("seats_taken = %d, want %d", taken, seats)
	}
}

// TestCapacityHoldsRace is the capacity-mode counterpart of
// TestBookingTablesRace and the reason the counter lives in the database: two
// concurrent transactions, each of which fits on its own, must not both commit.
//
// Remove chk_capacity_bucket_within_limit and this test fails — that is its
// job.
func TestCapacityHoldsRace(t *testing.T) {
	pool, ctx := setup(t)
	const seats = 10
	rid := seedCapacityVenue(t, pool, seats)
	capacity := NewCapacity(pool)
	txm := sqltx.NewManager(pool)
	start := time.Date(2026, 8, 5, 19, 0, 0, 0, time.UTC)

	// 6 seats each: either alone fits into 10, together they do not.
	ids := make([]uuid.UUID, 2)
	for i := range ids {
		ids[i] = seedCapacityBooking(t, pool, rid, start, 6).ID
	}
	// Overlapping but not identical windows, so the conflict is decided by the
	// shared buckets rather than by an identical row.
	starts := []time.Time{start, start.Add(2 * domain.CapacityBucket)}

	var (
		wg    sync.WaitGroup
		gate  = make(chan struct{})
		errCh = make(chan error, len(ids))
	)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			errCh <- txm.WithinTx(ctx, func(ctx context.Context) error {
				return capacity.Create(ctx, holdsFor(ids[i], rid, starts[i], 8, 6, seats))
			})
		}(i)
	}
	close(gate)
	wg.Wait()
	close(errCh)

	var wins, conflicts int
	for err := range errCh {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, domain.ErrAlreadyExists):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("race outcome: %d wins, %d conflicts; want exactly 1 and 1", wins, conflicts)
	}

	var taken int
	if err := pool.QueryRow(ctx,
		`SELECT max(seats_taken) FROM restaurant_capacity_buckets WHERE restaurant_id=$1`, rid).Scan(&taken); err != nil {
		t.Fatalf("read buckets: %v", err)
	}
	if taken > seats {
		t.Fatalf("venue oversold: %d seats taken of %d", taken, seats)
	}
}

// TestCapacityFreedOnCancel pins the second DB trigger: a cancelled booking
// releases its seats, and the guest behind it can then book them.
func TestCapacityFreedOnCancel(t *testing.T) {
	pool, ctx := setup(t)
	const seats = 4
	rid := seedCapacityVenue(t, pool, seats)
	bookings := New(pool)
	capacity := NewCapacity(pool)
	start := time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC)

	first := seedCapacityBooking(t, pool, rid, start, 4)
	if err := capacity.Create(ctx, holdsFor(first.ID, rid, start, 8, 4, seats)); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	second := seedCapacityBooking(t, pool, rid, start, 4)
	if err := capacity.Create(ctx, holdsFor(second.ID, rid, start, 8, 4, seats)); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("full venue = %v, want ErrAlreadyExists", err)
	}

	if err := bookings.UpdateStatus(ctx, first.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := capacity.Create(ctx, holdsFor(second.ID, rid, start, 8, 4, seats)); err != nil {
		t.Fatalf("after cancel the seats must be free again: %v", err)
	}

	usage, err := capacity.ListUsage(ctx, rid, start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("list usage: %v", err)
	}
	if len(usage) == 0 {
		t.Fatal("usage is empty after the second booking took the seats")
	}
	for _, u := range usage {
		if u.SeatsTaken != 4 || u.Free() != 0 {
			t.Fatalf("bucket %s: taken=%d free=%d, want 4/0", u.BucketStart, u.SeatsTaken, u.Free())
		}
	}
}

// TestCapacityLoweredLimitStillReleases guards the trap the release path was
// written around: after a venue lowers its declared capacity below what it has
// already sold, cancelling one of those bookings must still work. A naive
// implementation that refreshed seats_limit on the way down would leave the
// venue unable to cancel anything.
func TestCapacityLoweredLimitStillReleases(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedCapacityVenue(t, pool, 20)
	bookings := New(pool)
	capacity := NewCapacity(pool)
	start := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)

	b := seedCapacityBooking(t, pool, rid, start, 12)
	if err := capacity.Create(ctx, holdsFor(b.ID, rid, start, 8, 12, 20)); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	// The venue shrinks to 8 seats; the sold 12 stay sold.
	if _, err := pool.Exec(ctx,
		`UPDATE restaurant_capacity_buckets SET seats_limit=8 WHERE restaurant_id=$1 AND seats_taken=0`, rid); err != nil {
		t.Fatalf("shrink empty buckets: %v", err)
	}
	if err := bookings.UpdateStatus(ctx, b.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatalf("cancel under a lowered limit must still succeed: %v", err)
	}
	var taken int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(max(seats_taken),0) FROM restaurant_capacity_buckets WHERE restaurant_id=$1`, rid).Scan(&taken); err != nil {
		t.Fatalf("read buckets: %v", err)
	}
	if taken != 0 {
		t.Fatalf("seats_taken after cancel = %d, want 0", taken)
	}
}

// TestCapacityLeavesTableVenueAlone: a venue that books by tables must be
// untouched by any of this — no bucket rows appear for it, and its exclusion
// constraint keeps deciding on its own.
func TestCapacityLeavesTableVenueAlone(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	tid := seedTable(t, pool, rid)
	bookings := New(pool)
	tables := NewTables(pool)

	start := time.Date(2026, 8, 8, 19, 0, 0, 0, time.UTC)
	b := newBooking(rid, start)
	if err := bookings.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tables.Create(ctx, []domain.BookingTable{link(b.ID, tid, start, start.Add(2*time.Hour))}); err != nil {
		t.Fatalf("occupy table: %v", err)
	}
	var buckets int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM restaurant_capacity_buckets WHERE restaurant_id=$1`, rid).Scan(&buckets); err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if buckets != 0 {
		t.Fatalf("table-mode venue produced %d capacity buckets, want 0", buckets)
	}
}

// TestCapacityReplaceForBooking covers the amendment path: a booking moved in
// time releases its old buckets and claims the new ones inside one transaction.
func TestCapacityReplaceForBooking(t *testing.T) {
	pool, ctx := setup(t)
	const seats = 6
	rid := seedCapacityVenue(t, pool, seats)
	capacity := NewCapacity(pool)
	txm := sqltx.NewManager(pool)
	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	b := seedCapacityBooking(t, pool, rid, start, 6)
	if err := capacity.Create(ctx, holdsFor(b.ID, rid, start, 8, 6, seats)); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	moved := start.Add(4 * time.Hour)
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return capacity.ReplaceForBooking(ctx, b.ID, holdsFor(b.ID, rid, moved, 8, 6, seats))
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	var oldTaken, newTaken int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(sum(seats_taken),0) FROM restaurant_capacity_buckets
		 WHERE restaurant_id=$1 AND bucket_start < $2`, rid, moved).Scan(&oldTaken); err != nil {
		t.Fatalf("read old buckets: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(min(seats_taken),0) FROM restaurant_capacity_buckets
		 WHERE restaurant_id=$1 AND bucket_start >= $2 AND seats_taken > 0`, rid, moved).Scan(&newTaken); err != nil {
		t.Fatalf("read new buckets: %v", err)
	}
	if oldTaken != 0 || newTaken != 6 {
		t.Fatalf("after move: old=%d new=%d, want 0 and 6", oldTaken, newTaken)
	}

	// PeakTaken is what the admin API consults before lowering a capacity.
	peak, err := capacity.PeakTaken(ctx, rid, start)
	if err != nil || peak == nil {
		t.Fatalf("peak = %+v err=%v", peak, err)
	}
	if peak.SeatsTaken != 6 {
		t.Fatalf("peak seats = %d, want 6", peak.SeatsTaken)
	}
}

// TestCapacityRejectsAbsurdDeclaredCapacity pins the DB half of the admin
// validation: the bounds are also CHECKs, because this row gets edited by hand.
func TestCapacityRejectsAbsurdDeclaredCapacity(t *testing.T) {
	pool, ctx := setup(t)
	for _, seats := range []int{0, -5, 2001} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category, booking_capacity_mode, booking_capacity_seats)
			 VALUES ($1,'Bad','Алматы','₸','seats',$2)`, uuid.New(), seats); err == nil {
			t.Errorf("capacity %d was accepted by the database", seats)
		}
	}
	// 'seats' without a declared capacity is equally unrepresentable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, booking_capacity_mode)
		 VALUES ($1,'Bad','Алматы','₸','seats')`, uuid.New()); err == nil {
		t.Error("capacity mode without a capacity was accepted by the database")
	}
	// An unknown mode label is refused too.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, booking_capacity_mode)
		 VALUES ($1,'Bad','Алматы','₸','whatever')`, uuid.New()); err == nil {
		t.Error("unknown capacity mode was accepted by the database")
	}
}
