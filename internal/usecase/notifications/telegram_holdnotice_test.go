package notifications

import (
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// Pre-order-hold releases (spec preorder-hold-capture-on-confirm-20260924
// §criterion 25: "the venue notification about a booking with a hold carries
// the pre-order amount and D_venue") get a dedicated line the venue alert
// never had before. Mirrors the reasoning of
// TestCancelledGuestCopyDistinguishesPreorderHoldReasons: exercise the pure
// text builder directly, no dispatcher/DB involved.
func TestTelegramTextCarriesHoldAmountAndDeadlineInVenueTimezone(t *testing.T) {
	amount := int64(966000) // 9660.00 KZT
	// 08:30 UTC = 13:30 in Asia/Almaty (UTC+5) — chosen so a bug that renders
	// the deadline in UTC instead of the venue zone is caught by the exact
	// "13:30" match below, not silently right by coincidence.
	deadline := time.Date(2026, 8, 1, 8, 30, 0, 0, time.UTC)
	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load Asia/Almaty: %v", err)
	}

	e := Event{
		Type:                  domain.EventBookingCreated,
		GuestName:             "Дамир",
		Guests:                2,
		StartsAt:              time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC),
		HoldAmountMinor:       &amount,
		HoldCurrency:          domain.CurrencyKZT,
		VenueAnswerDeadlineAt: &deadline,
	}

	text := buildTelegramText(e, almaty)

	if !strings.Contains(text, "9660 ₸") {
		t.Fatalf("alert missing the hold amount:\n%s", text)
	}
	if !strings.Contains(text, "ответьте до 13:30") {
		t.Fatalf("alert missing the venue-timezone deadline (want 13:30 Asia/Almaty):\n%s", text)
	}
	// The existing fields must survive the addition untouched.
	if !strings.Contains(text, "Дамир") || !strings.Contains(text, "Гостей: 2") {
		t.Fatalf("alert lost its existing fields:\n%s", text)
	}
}

// An ordinary booking.created (no pre-order, or one whose payment is not a
// live hold) must not grow a money line out of nothing.
func TestTelegramTextOmitsHoldLineWhenNoHold(t *testing.T) {
	e := Event{
		Type:      domain.EventBookingCreated,
		GuestName: "Гость",
		Guests:    4,
		StartsAt:  time.Now(),
	}
	text := buildTelegramText(e, time.UTC)
	for _, forbidden := range []string{"заблокировано", "ответьте до"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("alert invented a hold line with no hold: %q in\n%s", forbidden, text)
		}
	}
}

// A cancellation must never carry the hold line even if the fields were
// somehow set on the event — it is a booking.created-only line, defensively
// checked in holdNoticeLine itself, not just by release.go never setting the
// fields elsewhere.
func TestTelegramTextOmitsHoldLineOnCancellation(t *testing.T) {
	amount := int64(500000)
	deadline := time.Now().Add(time.Hour)
	e := Event{
		Type:                  domain.EventBookingCancelled,
		CancelledBy:           domain.CancelledByGuest,
		GuestName:             "Гость",
		Guests:                2,
		StartsAt:              time.Now(),
		HoldAmountMinor:       &amount,
		VenueAnswerDeadlineAt: &deadline,
	}
	text := buildTelegramText(e, time.UTC)
	if strings.Contains(text, "заблокировано") || strings.Contains(text, "ответьте до") {
		t.Fatalf("cancellation must never carry the hold line:\n%s", text)
	}
}

// The JSON contract between the booking usecase (producer, release.go) and
// this dispatcher (consumer) must round-trip the two new fields, exactly like
// TestOutboxPayloadDecodesThePhone guards the existing ones.
func TestOutboxPayloadDecodesHoldNotice(t *testing.T) {
	e, err := toEvent(domain.BookingOutboxEvent{
		EventType: domain.EventBookingCreated,
		Payload: []byte(`{"restaurant_id":"5f5c8f52-1c1e-4a1e-9a4a-2d6f1a0f9d11",
			"name":"Дамир","guests":2,"starts_at":"2026-07-30T14:00:00Z",
			"hold_amount_minor":966000,"hold_currency":"KZT",
			"venue_answer_deadline_at":"2026-07-30T16:00:00Z"}`),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.HoldAmountMinor == nil || *e.HoldAmountMinor != 966000 {
		t.Fatalf("hold amount lost in decode: %v", e.HoldAmountMinor)
	}
	if e.HoldCurrency != domain.CurrencyKZT {
		t.Fatalf("hold currency lost in decode: %q", e.HoldCurrency)
	}
	if e.VenueAnswerDeadlineAt == nil || !e.VenueAnswerDeadlineAt.Equal(time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("venue answer deadline lost in decode: %v", e.VenueAnswerDeadlineAt)
	}
}
