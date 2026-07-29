package venuedashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
)

// seed inserts a venue and returns its id.
func seedVenue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, tz string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, timezone)
		 VALUES ($1,$2,'Алматы','₸',$3)`, id, name, tz); err != nil {
		t.Fatalf("seed venue %s: %v", name, err)
	}
	return id
}

func seedBooking(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	venue uuid.UUID, status string, guests int, createdAt, startsAt time.Time, reason *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, status,
		                       starts_at, ends_at, created_at, updated_at, source, cancellation_reason_code)
		 VALUES ($1,$2,'Гость','+77070000000','+77070000000',$3,$4,$5::timestamptz,
		         $5::timestamptz + interval '90 minutes',$6::timestamptz,$6::timestamptz,'app',$7)`,
		id, venue, guests, status, startsAt, createdAt, reason); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

// The tenant boundary of the whole feature: one venue's dashboard must never
// count another venue's bookings, even when both are busy in the same window.
func TestSummaryCountsOnlyTheRequestedVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	mine := seedVenue(t, ctx, pool, "Мой", "Asia/Almaty")
	theirs := seedVenue(t, ctx, pool, "Чужой", "Asia/Almaty")
	now := time.Now().UTC()

	seedBooking(t, ctx, pool, mine, "completed", 2, now.Add(-2*time.Hour), now.Add(24*time.Hour), nil)
	seedBooking(t, ctx, pool, theirs, "completed", 8, now.Add(-2*time.Hour), now.Add(24*time.Hour), nil)

	got, err := New(pool).Summary(ctx, mine, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("total = %d, want 1 (the other venue's booking leaked in)", got.Total)
	}
	if got.AvgPartySize != 2 {
		t.Fatalf("avg party = %v, want 2", got.AvgPartySize)
	}
}

// The number the venue actually cares about, and the one that is easiest to get
// wrong: cancellations AND no-shows both count as lost, and an empty period
// must read as 0%, never as a division by zero.
func TestCancelledShareCountsNoShowsAndSurvivesAnEmptyPeriod(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	now := time.Now().UTC()
	made := now.Add(-2 * time.Hour)

	for _, s := range []string{"completed", "confirmed", "cancelled", "cancelled", "no_show"} {
		seedBooking(t, ctx, pool, venue, s, 2, made, now.Add(24*time.Hour), nil)
	}

	repo := New(pool)
	got, err := repo.Summary(ctx, venue, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Total != 5 {
		t.Fatalf("total = %d, want 5", got.Total)
	}
	if got.CancelledShare != 60 {
		t.Fatalf("cancelled share = %v, want 60 (2 cancelled + 1 no-show of 5)", got.CancelledShare)
	}

	// A window with nothing in it: zeros, not NaN and not 100%.
	empty, err := repo.Summary(ctx, venue, now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("empty summary: %v", err)
	}
	if empty.Total != 0 || empty.CancelledShare != 0 || empty.AvgPartySize != 0 {
		t.Fatalf("an empty period must be all zeros, got %+v", empty)
	}
}

// Cancellations without a reason code are the venue's biggest blind spot, so
// they are reported as their own row instead of being dropped.
func TestCancelReasonsKeepTheUnexplainedOnes(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	now := time.Now().UTC()
	made := now.Add(-2 * time.Hour)
	plans := "changed_plans"

	seedBooking(t, ctx, pool, venue, "cancelled", 2, made, now.Add(24*time.Hour), &plans)
	seedBooking(t, ctx, pool, venue, "cancelled", 2, made, now.Add(24*time.Hour), &plans)
	seedBooking(t, ctx, pool, venue, "cancelled", 2, made, now.Add(24*time.Hour), nil)

	got, err := New(pool).Summary(ctx, venue, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(got.CancelReasons) != 2 {
		t.Fatalf("want two rows (a named reason and the unexplained one), got %+v", got.CancelReasons)
	}
	if got.CancelReasons[0].Reason != plans || got.CancelReasons[0].Count != 2 {
		t.Fatalf("most common reason first: %+v", got.CancelReasons[0])
	}
	var unexplained int
	for _, r := range got.CancelReasons {
		if r.Reason == "" {
			unexplained = r.Count
		}
	}
	if unexplained != 1 {
		t.Fatalf("the unexplained cancellation must be visible, got %+v", got.CancelReasons)
	}
}

// Load buckets by the RESERVED time in the venue's own zone. Read in UTC, an
// Almaty evening lands five hours earlier and the busiest hour looks empty.
func TestLoadBucketsInTheVenuesOwnTimezone(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	// 14:00 UTC is 19:00 in Almaty (UTC+5).
	starts := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
	seedBooking(t, ctx, pool, venue, "confirmed", 3, starts.Add(-48*time.Hour), starts, nil)
	// A cancelled booking must not occupy a slot on the load chart.
	seedBooking(t, ctx, pool, venue, "cancelled", 9, starts.Add(-48*time.Hour), starts, nil)

	slots, err := New(pool).Load(ctx, venue, starts.Add(-time.Hour), starts.Add(time.Hour))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("want one slot, got %+v", slots)
	}
	if slots[0].Hour != 19 {
		t.Fatalf("hour = %d, want 19 (local), not 14 (UTC)", slots[0].Hour)
	}
	if slots[0].Bookings != 1 || slots[0].Guests != 3 {
		t.Fatalf("a cancelled booking must not count: %+v", slots[0])
	}
}
