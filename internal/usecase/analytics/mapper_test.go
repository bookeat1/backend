package analytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// bookingPayloadWithPII mirrors the real booking_outbox payload the bookings
// usecase writes — it DOES carry name/phone/email. The mapper must never
// surface any of them.
func bookingPayloadWithPII(t *testing.T, userID *uuid.UUID, eventStatus string) (uuid.UUID, uuid.UUID, json.RawMessage) {
	t.Helper()
	bookingID := uuid.New()
	restaurantID := uuid.New()
	payload := map[string]any{
		"id":            bookingID,
		"restaurant_id": restaurantID,
		"user_id":       userID,
		"name":          "Damir Sarkulin",
		"phone":         "+77011234567",
		"email":         "guest@example.com",
		"guests":        4,
		"starts_at":     time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC),
		"status":        eventStatus,
		"source":        "guest_app",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return bookingID, restaurantID, raw
}

func assertNoPII(t *testing.T, ev Event) {
	t.Helper()
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ToLower(string(blob))
	for _, needle := range []string{"damir", "sarkulin", "77011234567", "guest@example.com", "name", "phone", "email"} {
		if strings.Contains(s, strings.ToLower(needle)) {
			t.Fatalf("PII leaked into analytics event (%q found): %s", needle, blob)
		}
	}
}

func TestMapBookingTracked_NoPII(t *testing.T) {
	userID := uuid.New()
	rowID := uuid.New()
	bookingID, restaurantID, payload := bookingPayloadWithPII(t, &userID, "confirmed")
	at := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)

	ev, tracked, err := mapRow(SourceBookingOutbox, SourceRow{
		ID: rowID, EventType: "booking.confirmed", Payload: payload, CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tracked {
		t.Fatal("booking.confirmed must be tracked")
	}
	if ev.Type != EventBookingConfirmed {
		t.Fatalf("type = %q, want booking_confirmed", ev.Type)
	}
	if ev.UserID != userID.String() {
		t.Fatalf("user_id = %q, want %q", ev.UserID, userID)
	}
	if ev.DeviceID != "bk-"+bookingID.String() {
		t.Fatalf("device_id = %q, want bk-%s", ev.DeviceID, bookingID)
	}
	if ev.InsertID != rowID.String() {
		t.Fatalf("insert_id = %q, want %q", ev.InsertID, rowID)
	}
	if !ev.Time.Equal(at) {
		t.Fatalf("time = %v, want %v", ev.Time, at)
	}
	if got := ev.Properties["restaurant_id"]; got != restaurantID.String() {
		t.Fatalf("restaurant_id property = %v, want %v", got, restaurantID)
	}
	if got := ev.Properties["guests"]; got != 4 {
		t.Fatalf("guests property = %v, want 4", got)
	}
	assertNoPII(t, ev)
}

func TestMapBookingNoUserID_EmptyUser(t *testing.T) {
	rowID := uuid.New()
	_, _, payload := bookingPayloadWithPII(t, nil, "pending")
	ev, tracked, err := mapRow(SourceBookingOutbox, SourceRow{
		ID: rowID, EventType: "booking.created", Payload: payload, CreatedAt: time.Now(),
	})
	if err != nil || !tracked {
		t.Fatalf("booking.created must be tracked; err=%v tracked=%v", err, tracked)
	}
	if ev.UserID != "" {
		t.Fatalf("user_id = %q, want empty for a booking with no account", ev.UserID)
	}
	if ev.DeviceID == "" || len(ev.DeviceID) < 5 {
		t.Fatalf("device_id %q must be a stable id >= 5 chars", ev.DeviceID)
	}
}

func TestMapBookingUntracked(t *testing.T) {
	_, _, payload := bookingPayloadWithPII(t, nil, "waitlisted")
	for _, et := range []string{"booking.waitlisted", "booking.arrived", "booking.completed", "booking.updated", "booking.message_created"} {
		_, tracked, err := mapRow(SourceBookingOutbox, SourceRow{
			ID: uuid.New(), EventType: et, Payload: payload, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("%s: unexpected error %v", et, err)
		}
		if tracked {
			t.Fatalf("%s must NOT be tracked in the initial set", et)
		}
	}
}

func TestMapBookingPoison(t *testing.T) {
	_, tracked, err := mapRow(SourceBookingOutbox, SourceRow{
		ID: uuid.New(), EventType: "booking.created", Payload: []byte("{not json"), CreatedAt: time.Now(),
	})
	if tracked {
		t.Fatal("a poison row must not be reported as tracked")
	}
	if err == nil {
		t.Fatal("a poison row must return a decode error")
	}
}

func TestMapPaymentTracked_NoPII_Bucket(t *testing.T) {
	rowID := uuid.New()
	bookingID := uuid.New()
	restaurantID := uuid.New()
	payload, err := json.Marshal(map[string]any{
		"id":            uuid.New(),
		"booking_id":    bookingID,
		"restaurant_id": restaurantID,
		"purpose":       "deposit",
		"status":        "captured",
		"amount_minor":  750000, // 7 500.00 -> bucket 5000_20000
		"currency":      "KZT",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, tracked, err := mapRow(SourcePaymentOutbox, SourceRow{
		ID: rowID, EventType: "payment.captured", Payload: payload, CreatedAt: time.Now(),
	})
	if err != nil || !tracked {
		t.Fatalf("payment.captured must be tracked; err=%v tracked=%v", err, tracked)
	}
	if ev.Type != EventPaymentCaptured {
		t.Fatalf("type = %q, want payment_captured", ev.Type)
	}
	if ev.DeviceID != "bk-"+bookingID.String() {
		t.Fatalf("device_id = %q, want bk-%s", ev.DeviceID, bookingID)
	}
	if got := ev.Properties["amount_bucket"]; got != "5000_20000" {
		t.Fatalf("amount_bucket = %v, want 5000_20000", got)
	}
	if _, ok := ev.Properties["amount_minor"]; ok {
		t.Fatal("exact amount_minor must NOT be shipped, only the coarse bucket")
	}
	assertNoPII(t, ev)
}

func TestMapPaymentUntracked(t *testing.T) {
	payload := []byte(`{"booking_id":"` + uuid.New().String() + `","amount_minor":0}`)
	for _, et := range []string{"payment.created", "payment.authorized", "payment.voided", "payment.failed", "payment.settled", "payment.partially_refunded"} {
		_, tracked, err := mapRow(SourcePaymentOutbox, SourceRow{
			ID: uuid.New(), EventType: et, Payload: payload, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("%s: unexpected error %v", et, err)
		}
		if tracked {
			t.Fatalf("%s must NOT be tracked in the initial set", et)
		}
	}
}

func TestAmountBucket(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "0"}, {-5, "0"}, {100, "lt_5000"}, {499_999, "lt_5000"},
		{500_000, "5000_20000"}, {1_999_999, "5000_20000"},
		{2_000_000, "20000_100000"}, {9_999_999, "20000_100000"},
		{10_000_000, "gte_100000"}, {50_000_000, "gte_100000"},
	}
	for _, c := range cases {
		if got := amountBucket(c.minor); got != c.want {
			t.Fatalf("amountBucket(%d) = %q, want %q", c.minor, got, c.want)
		}
	}
}
