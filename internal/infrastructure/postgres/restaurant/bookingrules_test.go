package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestGetBookingRulesOverride(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Booking Rules Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A fresh venue: no override, and the money-path window is whatever the
	// column's own migration default left it at (120, migration 0035).
	o, freeCancelMin, err := repo.GetBookingRulesOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.HoldMinutes != nil || o.LateArrivalText != nil || o.LateArrivalTextI18n != nil {
		t.Errorf("fresh venue override = %+v, want all nil", o)
	}
	if freeCancelMin == nil || *freeCancelMin != 120 {
		t.Errorf("free_cancel_window_minutes = %v, want the column default 120", freeCancelMin)
	}
}

// UpdateBookingRules is exercised end to end: set, read back, clear, read
// back again — the column-level behaviour a unit test over a fake repo cannot
// prove (in particular the CHECK constraint and the "0/empty clears" contract).
func TestUpdateBookingRules(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Booking Rules Bistro 2", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	hold, text := 25, "Опоздали? Звоните хостес."
	i18n := domain.I18n{"ru": text, "kk": "Кешіктіңіз бе?"}
	if err := repo.UpdateBookingRules(ctx, m.ID, domain.BookingRulesOverride{
		HoldMinutes: &hold, LateArrivalText: &text, LateArrivalTextI18n: i18n,
	}, true); err != nil {
		t.Fatalf("update: %v", err)
	}

	o, _, err := repo.GetBookingRulesOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if o.HoldMinutes == nil || *o.HoldMinutes != 25 {
		t.Errorf("HoldMinutes = %v, want 25", o.HoldMinutes)
	}
	if o.LateArrivalText == nil || *o.LateArrivalText != text {
		t.Errorf("LateArrivalText = %v, want %q", o.LateArrivalText, text)
	}
	if o.LateArrivalTextI18n["kk"] != "Кешіктіңіз бе?" {
		t.Errorf("LateArrivalTextI18n[kk] = %q, want the stored translation", o.LateArrivalTextI18n["kk"])
	}

	// A booking policy update touching only HoldMinutes must not disturb the
	// unrelated text — same "only the provided columns move" contract as
	// UpdateBookingPolicy.
	newHold := 40
	if err := repo.UpdateBookingRules(ctx, m.ID, domain.BookingRulesOverride{HoldMinutes: &newHold}, false); err != nil {
		t.Fatalf("update hold only: %v", err)
	}
	o, _, err = repo.GetBookingRulesOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after hold-only update: %v", err)
	}
	if o.HoldMinutes == nil || *o.HoldMinutes != 40 {
		t.Errorf("HoldMinutes = %v, want 40", o.HoldMinutes)
	}
	if o.LateArrivalText == nil || *o.LateArrivalText != text {
		t.Errorf("LateArrivalText = %v, want it left untouched by the hold-only update", o.LateArrivalText)
	}

	// 0 / "" clear the override back to the platform default (NULL), and
	// clearing the text clears its translations in the SAME statement — the
	// CHECK (late_arrival_text_i18n needs late_arrival_text) must never see a
	// half-cleared row.
	zero, empty := 0, ""
	if err := repo.UpdateBookingRules(ctx, m.ID, domain.BookingRulesOverride{
		HoldMinutes: &zero, LateArrivalText: &empty,
	}, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	o, _, err = repo.GetBookingRulesOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if o.HoldMinutes != nil {
		t.Errorf("HoldMinutes = %v, want nil after clearing", o.HoldMinutes)
	}
	if o.LateArrivalText != nil {
		t.Errorf("LateArrivalText = %v, want nil after clearing", o.LateArrivalText)
	}
	if o.LateArrivalTextI18n != nil {
		t.Errorf("LateArrivalTextI18n = %v, want nil after clearing", o.LateArrivalTextI18n)
	}
}

// A translation-only call (i18nTouched=true, LateArrivalText nil) must merge
// the map without disturbing the base text — the read path GetByID uses.
func TestUpdateBookingRulesTranslationOnlyLeavesBaseTextAlone(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Booking Rules Bistro 3", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	text := "Звоните"
	if err := repo.UpdateBookingRules(ctx, m.ID,
		domain.BookingRulesOverride{LateArrivalText: &text, LateArrivalTextI18n: domain.I18n{"ru": text}}, true,
	); err != nil {
		t.Fatalf("set base text: %v", err)
	}

	if err := repo.UpdateBookingRules(ctx, m.ID,
		domain.BookingRulesOverride{LateArrivalTextI18n: domain.I18n{"ru": text, "kk": "Қоңырау шалыңыз"}}, true,
	); err != nil {
		t.Fatalf("translate: %v", err)
	}

	o, _, err := repo.GetBookingRulesOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.LateArrivalText == nil || *o.LateArrivalText != text {
		t.Errorf("LateArrivalText = %v, want the base text untouched", o.LateArrivalText)
	}
	if o.LateArrivalTextI18n["kk"] != "Қоңырау шалыңыз" {
		t.Errorf("LateArrivalTextI18n[kk] = %q, want the new translation", o.LateArrivalTextI18n["kk"])
	}
}

// GetByID (the detail read the venue DTO and the confirmation payload build
// on) must surface the SAME columns GetBookingRulesOverride reads, plus the
// money-path window under Restaurant.FreeCancelWindowMinutes.
func TestGetByIDIncludesBookingRulesAndFreeCancelWindow(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Booking Rules Bistro 4", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	hold := 30
	if err := repo.UpdateBookingRules(ctx, m.ID, domain.BookingRulesOverride{HoldMinutes: &hold}, false); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET free_cancel_window_minutes=90 WHERE id=$1`, m.ID); err != nil {
		t.Fatalf("seed free cancel window: %v", err)
	}

	agg, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if agg.BookingRules.HoldMinutes == nil || *agg.BookingRules.HoldMinutes != 30 {
		t.Errorf("BookingRules.HoldMinutes = %v, want 30", agg.BookingRules.HoldMinutes)
	}
	if agg.FreeCancelWindowMinutes == nil || *agg.FreeCancelWindowMinutes != 90 {
		t.Errorf("FreeCancelWindowMinutes = %v, want 90", agg.FreeCancelWindowMinutes)
	}
}
