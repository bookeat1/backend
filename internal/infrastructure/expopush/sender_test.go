package expopush

import (
	"context"
	"io"
	"net/http"
	"testing"

	"backend-core/internal/usecase/notifications"
)

func testMessage() notifications.MobilePushMessage {
	return notifications.MobilePushMessage{Title: "Бронь подтверждена", Body: "«Ocean Basket» · 01.09 в 19:30"}
}

// THE TICKET ID MUST COME BACK. Expo's `ok` ticket only means "accepted"; the id
// is the only handle that can ever answer "did the device actually get it".
// Dropping it (the behaviour before this change) is what made a delayed
// DeviceNotRegistered invisible: on 2026-09-01 three live android tokens all
// returned `ok` here and two returned DeviceNotRegistered from getReceipts.
func TestSendReturnsTicketID(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"status":"ok","id":"XXXX-YYYY-ZZZZ"}]}`)
	})
	got, err := s.Send(context.Background(), "ExponentPushToken[abc]", testMessage())
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Verdict != notifications.MobilePushDelivered {
		t.Fatalf("verdict = %v, want Delivered", got.Verdict)
	}
	if got.TicketID != "XXXX-YYYY-ZZZZ" {
		t.Fatalf("ticket id = %q, want the id Expo returned", got.TicketID)
	}
}

// A ticket that already declares the device dead has no receipt to poll: the
// token is deactivated on the spot, so no id must be handed back.
func TestSendDeviceNotRegisteredInTicketCarriesNoTicketID(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w,
			`{"data":[{"status":"error","message":"not registered","details":{"error":"DeviceNotRegistered"},"id":"ignored"}]}`)
	})
	got, err := s.Send(context.Background(), "ExponentPushToken[abc]", testMessage())
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Verdict != notifications.MobilePushDeviceGone {
		t.Fatalf("verdict = %v, want DeviceGone", got.Verdict)
	}
	if got.TicketID != "" {
		t.Fatalf("ticket id = %q, want none for a device already known to be gone", got.TicketID)
	}
}

// Any other ticket error is not retryable and has nothing to poll.
func TestSendRejectedCarriesNoTicketID(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w,
			`{"data":[{"status":"error","message":"too big","details":{"error":"MessageTooBig"}}]}`)
	})
	got, err := s.Send(context.Background(), "ExponentPushToken[abc]", testMessage())
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Verdict != notifications.MobilePushRejected || got.TicketID != "" {
		t.Fatalf("result = %+v, want Rejected with no ticket", got)
	}
}

// 5xx stays a transient error the notifier retries, and carries no ticket.
func TestSendTransientStatusIsAnError(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	got, err := s.Send(context.Background(), "ExponentPushToken[abc]", testMessage())
	if err == nil {
		t.Fatal("a 502 was reported as success")
	}
	if got.TicketID != "" {
		t.Fatalf("ticket id = %q on a failed send", got.TicketID)
	}
}
