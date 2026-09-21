package notification

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestVenuesBookingRules pins the guest-push footer's read: a fresh venue has
// no override (nil fields) and the money-path window is whatever the
// column's own default left it at; once set, both the override and its
// translations round-trip byte for byte.
func TestVenuesBookingRules(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	rid := seedRestaurant(t, pool)
	venues := NewVenues(pool)
	ctx := context.Background()

	o, freeCancelMin, err := venues.BookingRules(ctx, rid)
	if err != nil {
		t.Fatalf("booking rules: %v", err)
	}
	if o.HoldMinutes != nil || o.LateArrivalText != nil || o.LateArrivalTextI18n != nil {
		t.Errorf("fresh venue override = %+v, want all nil", o)
	}
	if freeCancelMin == nil || *freeCancelMin != 120 {
		t.Errorf("free_cancel_window_minutes = %v, want the column default 120", freeCancelMin)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE restaurants SET hold_minutes=20, late_arrival_text=$2,
		    late_arrival_text_i18n=$3, free_cancel_window_minutes=90 WHERE id=$1`,
		rid, "Позвоните нам", []byte(`{"ru":"Позвоните нам","kk":"Бізге қоңырау шалыңыз"}`)); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	o, freeCancelMin, err = venues.BookingRules(ctx, rid)
	if err != nil {
		t.Fatalf("booking rules after seed: %v", err)
	}
	if o.HoldMinutes == nil || *o.HoldMinutes != 20 {
		t.Errorf("HoldMinutes = %v, want 20", o.HoldMinutes)
	}
	if o.LateArrivalText == nil || *o.LateArrivalText != "Позвоните нам" {
		t.Errorf("LateArrivalText = %v, want the stored text", o.LateArrivalText)
	}
	if o.LateArrivalTextI18n["kk"] != "Бізге қоңырау шалыңыз" {
		t.Errorf("LateArrivalTextI18n[kk] = %q, want the stored translation", o.LateArrivalTextI18n["kk"])
	}
	if freeCancelMin == nil || *freeCancelMin != 90 {
		t.Errorf("free_cancel_window_minutes = %v, want 90", freeCancelMin)
	}
}

// A missing venue must be reported as ErrNotFound, not a zero-value success —
// otherwise a stale/incorrect restaurant_id silently omits the footer instead
// of surfacing the bug.
func TestVenuesBookingRulesMissingVenue(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	venues := NewVenues(pool)

	_, _, err := venues.BookingRules(context.Background(), uuid.New())
	if err != domain.ErrNotFound {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}
