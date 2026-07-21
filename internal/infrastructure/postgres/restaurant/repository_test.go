package restaurant

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestRestaurantCRUDAndList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	order := 1
	popular := true
	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Test Bistro", NameI18n: domain.I18n{"ru": "Бистро"},
		City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		IsActive: true, IsPopular: &popular, DisplayOrder: &order,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Test Bistro" || got.NameI18n["ru"] != "Бистро" || got.City != domain.CityAlmaty {
		t.Errorf("roundtrip mismatch: %+v", got.Restaurant)
	}

	items, total, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), IsPopular: &popular})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != m.ID {
		t.Errorf("list = %d items (total %d), want 1", len(items), total)
	}

	if err := repo.SetActive(ctx, m.ID, false); err != nil {
		t.Fatalf("set active: %v", err)
	}
	_, total, _ = repo.ListActive(ctx, domain.RestaurantFilter{})
	if total != 0 {
		t.Errorf("after deactivate total = %d, want 0", total)
	}

	if _, err := repo.GetByID(ctx, uuid.New()); err != domain.ErrNotFound {
		t.Errorf("missing get err = %v, want ErrNotFound", err)
	}
}

func TestRepositoryUpdate(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Original Name", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Postgres timestamptz keeps microseconds, Go time.Time keeps nanoseconds:
	// compare against the value as the database stores it.
	createdAt := m.CreatedAt.Truncate(time.Microsecond)

	upd := &domain.Restaurant{
		ID: m.ID, Name: "Updated Name", City: domain.CityAstana,
		PriceCategory: domain.PriceHigh, IsActive: false,
		// CreatedAt intentionally left zero, as it would be on a request DTO;
		// Update must not write it to the created_at column.
	}
	if err := repo.Update(ctx, upd); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Updated Name" || got.City != domain.CityAstana || got.PriceCategory != domain.PriceHigh || got.IsActive {
		t.Errorf("update did not persist: %+v", got.Restaurant)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at changed: got %v, want %v", got.CreatedAt, createdAt)
	}

	if err := repo.Update(ctx, &domain.Restaurant{ID: uuid.New(), Name: "x", City: domain.CityAlmaty, PriceCategory: domain.PriceLow}); err != domain.ErrNotFound {
		t.Errorf("update missing err = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestUpdateBookingPolicy covers the write half of the wave-3 policy columns
// and, via GetByID, the read half — a regression guard for the period where
// the overrides could only be set by hand in SQL.
func TestUpdateBookingPolicy(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Policy Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A fresh row inherits everything: all override columns are NULL.
	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p := got.BookingPolicy; p.AutoConfirm != nil || p.Timezone != nil || p.ConfirmSLAMinutes != nil {
		t.Fatalf("fresh restaurant has overrides: %+v", p)
	}

	// First patch: a subset only.
	if err := repo.UpdateBookingPolicy(ctx, m.ID, domain.BookingPolicyOverride{
		AutoConfirm:            boolPtr(false),
		ConfirmSLAMinutes:      intPtr(45),
		BookingBufferMinutes:   intPtr(0),
		BookingDurationMinutes: intPtr(90),
	}); err != nil {
		t.Fatalf("patch policy: %v", err)
	}
	got, err = repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after patch: %v", err)
	}
	p := got.BookingPolicy
	if p.AutoConfirm == nil || *p.AutoConfirm {
		t.Errorf("auto_confirm = %v, want false", p.AutoConfirm)
	}
	if p.ConfirmSLAMinutes == nil || *p.ConfirmSLAMinutes != 45 {
		t.Errorf("confirm_sla_minutes = %v, want 45", p.ConfirmSLAMinutes)
	}
	if p.BookingBufferMinutes == nil || *p.BookingBufferMinutes != 0 {
		t.Errorf("buffer = %v, want an explicit 0", p.BookingBufferMinutes)
	}
	if p.BookingDurationMinutes == nil || *p.BookingDurationMinutes != 90 {
		t.Errorf("duration = %v, want 90", p.BookingDurationMinutes)
	}
	// Fields never sent stay NULL (= inherit the env default).
	if p.Timezone != nil || p.BookingHorizonDays != nil || p.MaxGuestsPerBooking != nil ||
		p.BookingLeadMinutes != nil || p.CancelDeadlineMinutes != nil {
		t.Errorf("unsent columns were written: %+v", p)
	}

	// Second patch: touches other columns; the first patch's values survive.
	if err := repo.UpdateBookingPolicy(ctx, m.ID, domain.BookingPolicyOverride{
		Timezone:            strPtr("Europe/Berlin"),
		MaxGuestsPerBooking: intPtr(8),
	}); err != nil {
		t.Fatalf("second patch: %v", err)
	}
	got, err = repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after second patch: %v", err)
	}
	p = got.BookingPolicy
	if p.Timezone == nil || *p.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %v, want Europe/Berlin", p.Timezone)
	}
	if p.MaxGuestsPerBooking == nil || *p.MaxGuestsPerBooking != 8 {
		t.Errorf("max guests = %v, want 8", p.MaxGuestsPerBooking)
	}
	if p.AutoConfirm == nil || *p.AutoConfirm {
		t.Errorf("auto_confirm lost by the second patch: %v", p.AutoConfirm)
	}
	if p.ConfirmSLAMinutes == nil || *p.ConfirmSLAMinutes != 45 {
		t.Errorf("confirm_sla lost by the second patch: %v", p.ConfirmSLAMinutes)
	}
	if p.BookingHorizonDays != nil || p.BookingLeadMinutes != nil {
		t.Errorf("still-unsent columns were written: %+v", p)
	}

	// A patch does not disturb the rest of the row.
	if got.Name != "Policy Bistro" || !got.IsActive {
		t.Errorf("base columns changed: %+v", got.Restaurant)
	}

	// Empty patch: no-op, but a missing venue must still be reported.
	if err := repo.UpdateBookingPolicy(ctx, m.ID, domain.BookingPolicyOverride{}); err != nil {
		t.Errorf("empty patch on existing venue: %v", err)
	}
	if err := repo.UpdateBookingPolicy(ctx, uuid.New(), domain.BookingPolicyOverride{}); err != domain.ErrNotFound {
		t.Errorf("empty patch on unknown venue = %v, want ErrNotFound", err)
	}
	if err := repo.UpdateBookingPolicy(ctx, uuid.New(), domain.BookingPolicyOverride{
		AutoConfirm: boolPtr(true),
	}); err != domain.ErrNotFound {
		t.Errorf("patch on unknown venue = %v, want ErrNotFound", err)
	}
}

func intPtr(v int) *int       { return &v }
func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
