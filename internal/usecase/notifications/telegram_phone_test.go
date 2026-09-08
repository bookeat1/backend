package notifications

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The venue alert has to be actionable on its own. Without the number staff
// cannot confirm a large party or chase a no-show without opening the panel,
// which is the whole reason the alert exists.
func TestTelegramTextCarriesTheGuestPhone(t *testing.T) {
	text := buildTelegramText(Event{
		GuestName:  "Дамир",
		GuestPhone: "+77078692233",
		Guests:     2,
		StartsAt:   time.Date(2026, 7, 30, 19, 0, 0, 0, time.UTC),
	})

	if !strings.Contains(text, "+77078692233") {
		t.Fatalf("phone missing from the alert:\n%s", text)
	}
	if !strings.Contains(text, "Дамир") || !strings.Contains(text, "Гостей: 2") {
		t.Fatalf("alert lost its existing fields:\n%s", text)
	}
}

// A booking can exist without a number (staff-entered, or a guest who left the
// field empty). The line is dropped rather than printed empty: "Телефон:" with
// nothing after it reads as a broken message, not as missing data.
func TestTelegramTextOmitsAnEmptyPhoneLine(t *testing.T) {
	text := buildTelegramText(Event{GuestName: "Гость", Guests: 4, StartsAt: time.Now()})

	if strings.Contains(text, "Телефон") {
		t.Fatalf("empty phone must not print a label:\n%s", text)
	}
}

// The phone travels in the outbox payload and was simply not decoded. This
// guards the decode, which is where it went missing.
func TestOutboxPayloadDecodesThePhone(t *testing.T) {
	e, err := toEvent(domain.BookingOutboxEvent{
		ID:        uuid.New(),
		BookingID: uuid.New(),
		EventType: domain.EventBookingCreated,
		Payload: []byte(`{"restaurant_id":"5f5c8f52-1c1e-4a1e-9a4a-2d6f1a0f9d11",
			"name":"Дамир","phone":"+77078692233","guests":2,"starts_at":"2026-07-30T14:00:00Z"}`),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.GuestPhone != "+77078692233" {
		t.Fatalf("phone lost in decode: %q", e.GuestPhone)
	}
}
