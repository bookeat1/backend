package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestKitchenAttentionRouting(t *testing.T) {
	et := domain.EventBookingKitchenOrderAttention
	if !(&TelegramNotifier{}).Interested(et) || !(&WebPushNotifier{}).Interested(et) {
		t.Fatal("staff channels must take the kitchen alert")
	}
	if (&WhatsAppNotifier{}).Interested(et) || (&GuestPushNotifier{}).Interested(et) || (&FeedNotifier{}).Interested(et) {
		t.Fatal("guest channels and WhatsApp must not take the kitchen alert")
	}
}

func TestKitchenAttentionEventAndText(t *testing.T) {
	rid := uuid.New()
	payload, _ := json.Marshal(map[string]any{"restaurant_id": rid, "name": "Аня", "starts_at": time.Now(),
		"reason": "cancel_failed", "error_text": strings.Repeat("я", 300)})
	e, err := toEvent(domain.BookingOutboxEvent{ID: uuid.New(), BookingID: uuid.New(), EventType: domain.EventBookingKitchenOrderAttention, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if e.RestaurantID != rid || e.KitchenReason != "cancel_failed" {
		t.Fatalf("%+v", e)
	}
	txt := buildTelegramText(e, time.UTC)
	if !strings.Contains(txt, "вручную") || strings.Contains(txt, strings.Repeat("я", 250)) {
		t.Fatalf("text: %s", txt)
	}
	if buildPayload(e).Body == "" {
		t.Fatal("empty push body")
	}
}
