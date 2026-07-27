package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func ptr[T any](v T) *T { return &v }

func TestScheduleOverrideUpsertListDelete(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// closed-day override
	closed := &domain.ScheduleOverride{RestaurantID: rid, Date: day, IsClosed: true, Note: ptr("New Year")}
	if err := repo.Upsert(ctx, closed); err != nil {
		t.Fatalf("upsert closed: %v", err)
	}
	// idempotent upsert on the same (restaurant, date): flip to an open, custom-hours day
	open := &domain.ScheduleOverride{
		RestaurantID: rid, Date: day, IsClosed: false, OpenTime: ptr("12:00"), CloseTime: ptr("18:00"),
	}
	if err := repo.Upsert(ctx, open); err != nil {
		t.Fatalf("upsert open (replace): %v", err)
	}

	list, err := repo.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("upsert should replace not duplicate: got %d rows", len(list))
	}
	got := list[0]
	if got.IsClosed || got.OpenTime == nil || *got.OpenTime != "12:00" || *got.CloseTime != "18:00" {
		t.Errorf("replaced override mismatch: %+v", got)
	}

	// CHECK-constraint mapping: an "open" override with no times → ErrValidation
	bad := &domain.ScheduleOverride{RestaurantID: rid, Date: day.AddDate(0, 0, 1), IsClosed: false}
	if err := repo.Upsert(ctx, bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad override: got %v, want ErrValidation", err)
	}

	// delete reverts the day; deleting again is ErrNotFound
	if err := repo.Delete(ctx, rid, day); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Delete(ctx, rid, day); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}

// TestScheduleOverridePaidFieldsAndInstantLookup covers the migration-0036
// paid-booking fields plus the timezone-correct instant lookup the payments
// path uses.
func TestScheduleOverridePaidFieldsAndInstantLookup(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	// Venue in Asia/Almaty (UTC+5): a booking just after local midnight on the
	// 1st is still 2025-12-31 in UTC, so the calendar-date match must be done in
	// the venue's zone, not UTC.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, timezone) VALUES ($1,'R','Алматы','₸','Asia/Almaty')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	paid := &domain.ScheduleOverride{
		RestaurantID: rid, Date: day, IsClosed: false, OpenTime: ptr("10:00"), CloseTime: ptr("23:00"),
		BookingPaymentRequired: true, DepositAmountMinor: ptr(int64(750_000)),
	}
	if err := repo.Upsert(ctx, paid); err != nil {
		t.Fatalf("upsert paid: %v", err)
	}

	// Round-trip via list.
	list, err := repo.ListByRestaurant(ctx, rid)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: err=%v n=%d", err, len(list))
	}
	if !list[0].BookingPaymentRequired || list[0].DepositAmountMinor == nil || *list[0].DepositAmountMinor != 750_000 {
		t.Fatalf("paid fields not persisted: %+v", list[0])
	}

	// 2026-01-01 01:00 Almaty == 2025-12-31 20:00 UTC → matches the 1 Jan override.
	inZone := time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC)
	o, err := repo.GetForBookingInstant(ctx, rid, inZone, "UTC")
	if err != nil {
		t.Fatalf("GetForBookingInstant (in zone): %v", err)
	}
	if !o.BookingPaymentRequired || *o.DepositAmountMinor != 750_000 {
		t.Fatalf("instant lookup returned wrong override: %+v", o)
	}

	// 2026-01-02 01:00 Almaty == 2026-01-01 20:00 UTC → NO override for 2 Jan.
	nextDay := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	if _, err := repo.GetForBookingInstant(ctx, rid, nextDay, "UTC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("instant lookup next day: got %v, want ErrNotFound", err)
	}

	// CHECK: booking_payment_required=true with a NULL amount must be rejected.
	badPaid := &domain.ScheduleOverride{
		RestaurantID: rid, Date: day.AddDate(0, 0, 5), IsClosed: false,
		OpenTime: ptr("10:00"), CloseTime: ptr("23:00"), BookingPaymentRequired: true,
	}
	if err := repo.Upsert(ctx, badPaid); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("paid override with NULL amount: got %v, want ErrValidation", err)
	}
}

// TestScheduleOverrideInstantFallbackTZ: a venue with no stored timezone falls
// back to the platform default zone for the date match.
func TestScheduleOverrideInstantFallbackTZ(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	// No timezone column set (NULL).
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)
	day := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)
	o := &domain.ScheduleOverride{
		RestaurantID: rid, Date: day, IsClosed: false, OpenTime: ptr("10:00"), CloseTime: ptr("23:00"),
		BookingPaymentRequired: true, DepositAmountMinor: ptr(int64(300_000)),
	}
	if err := repo.Upsert(ctx, o); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 2026-03-21 02:00 Almaty (UTC+5) == 2026-03-20 21:00 UTC → matches when the
	// fallback zone Asia/Almaty is applied (a UTC match would resolve to 20 Mar).
	instant := time.Date(2026, 3, 20, 21, 0, 0, 0, time.UTC)
	got, err := repo.GetForBookingInstant(ctx, rid, instant, "Asia/Almaty")
	if err != nil {
		t.Fatalf("GetForBookingInstant (fallback tz): %v", err)
	}
	if !got.BookingPaymentRequired || *got.DepositAmountMinor != 300_000 {
		t.Fatalf("fallback-tz lookup wrong: %+v", got)
	}
}

// ListForVenues is the batch read behind the public catalog. It must group by
// venue, respect the INCLUSIVE date bounds, and — the part that is easy to get
// wrong — bind the bounds as calendar dates, not as instants reinterpreted in
// whatever timezone the connection happens to run in.
func TestScheduleOverrideListForVenues(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	var rids []uuid.UUID
	for i := 0; i < 2; i++ {
		rid := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
			t.Fatalf("seed restaurant: %v", err)
		}
		rids = append(rids, rid)
	}
	repo := New(pool)
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }

	seed := []*domain.ScheduleOverride{
		{RestaurantID: rids[0], Date: d(1), IsClosed: true},
		{RestaurantID: rids[0], Date: d(5), OpenTime: ptr("16:00"), CloseTime: ptr("20:00")},
		{RestaurantID: rids[0], Date: d(20), IsClosed: true}, // outside the window
		{RestaurantID: rids[1], Date: d(3), IsClosed: true},
	}
	for _, o := range seed {
		if err := repo.Upsert(ctx, o); err != nil {
			t.Fatalf("seed override: %v", err)
		}
	}

	got, err := repo.ListForVenues(ctx, rids, d(1), d(5))
	if err != nil {
		t.Fatalf("list for venues: %v", err)
	}
	if len(got[rids[0]]) != 2 {
		t.Errorf("venue 0: %+v, want the two inside [01-01, 01-05] (bounds inclusive)", got[rids[0]])
	}
	if len(got[rids[1]]) != 1 {
		t.Errorf("venue 1: %+v, want its single override", got[rids[1]])
	}
	for _, o := range got[rids[0]] {
		if o.RestaurantID != rids[0] {
			t.Errorf("rows leaked across venues: %+v", o)
		}
	}
	// The stored date must survive the round-trip as the same calendar day.
	if len(got[rids[0]]) > 0 && got[rids[0]][0].Date.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("date came back as %s, want 2026-01-01", got[rids[0]][0].Date.Format(time.RFC3339))
	}

	// A window that touches nothing returns an empty map, not every row.
	empty, err := repo.ListForVenues(ctx, rids, d(10), d(12))
	if err != nil {
		t.Fatalf("list empty window: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("window with no overrides returned %+v", empty)
	}
	// No ids at all: no query, no rows.
	if m, err := repo.ListForVenues(ctx, nil, d(1), d(5)); err != nil || len(m) != 0 {
		t.Errorf("empty id list = %v, %v; want an empty map and no error", m, err)
	}
}

// The session timezone must not move a date. The same window is asked for on a
// connection running in UTC and one running in Asia/Almaty; both must return
// the boundary row, which a `$1::timestamptz::date` binding would drop.
func TestScheduleOverrideListForVenuesIgnoresSessionTimezone(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_schedule_overrides", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := New(pool).Upsert(ctx, &domain.ScheduleOverride{RestaurantID: rid, Date: day, IsClosed: true}); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	for _, tz := range []string{"UTC", "Asia/Almaty", "Pacific/Honolulu"} {
		t.Run(tz, func(t *testing.T) {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "SET TIME ZONE '"+tz+"'"); err != nil {
				t.Fatalf("set tz: %v", err)
			}
			out, err := New(conn).ListForVenues(ctx, []uuid.UUID{rid}, day, day)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(out[rid]) != 1 {
				t.Errorf("session tz %s lost the boundary row: %+v", tz, out)
			}
			if _, err := conn.Exec(ctx, "SET TIME ZONE 'UTC'"); err != nil {
				t.Fatalf("reset tz: %v", err)
			}
		})
	}
}
