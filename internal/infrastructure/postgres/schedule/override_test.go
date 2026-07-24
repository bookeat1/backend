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
