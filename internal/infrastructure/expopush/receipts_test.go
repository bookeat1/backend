package expopush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend-core/internal/usecase/notifications"
)

// newTestSender points a Sender at a stub Expo.
func newTestSender(t *testing.T, h http.HandlerFunc) *Sender {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewSender(Config{Endpoint: srv.URL + "/send", ReceiptsEndpoint: srv.URL + "/receipts"})
}

// THE RESPONSE SHAPE, verbatim from a live call on 2026-09-01: `data` is an
// OBJECT keyed by ticket id, an ok receipt carries only a status, and a dead
// device is an error whose details.error is DeviceNotRegistered.
func TestReceiptsParsesLiveResponseShape(t *testing.T) {
	const body = `{"data":{
		"aaaa-1111":{"status":"ok"},
		"bbbb-2222":{"status":"error","message":"The recipient device is not registered with FCM.","details":{"error":"DeviceNotRegistered"}},
		"cccc-3333":{"status":"error","message":"Message too big","details":{"error":"MessageTooBig"}}
	}}`
	var gotBody receiptsRequest
	s := newTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body is not {\"ids\":[…]}: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})

	// "dddd-4444" is asked about but ABSENT from the answer: Expo has no receipt
	// for it yet. Observed live — the caller must be able to tell that apart
	// from a delivery.
	got, err := s.Receipts(context.Background(), []string{"aaaa-1111", "bbbb-2222", "cccc-3333", "dddd-4444"})
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if len(gotBody.IDs) != 4 {
		t.Fatalf("sent ids = %v, want all four", gotBody.IDs)
	}
	if len(got) != 3 {
		t.Fatalf("receipts = %v, want exactly the three Expo answered about", got)
	}
	if got["aaaa-1111"].Verdict != notifications.MobilePushDelivered {
		t.Fatalf("ok receipt = %+v, want Delivered", got["aaaa-1111"])
	}
	if got["bbbb-2222"].Verdict != notifications.MobilePushDeviceGone {
		t.Fatalf("DeviceNotRegistered receipt = %+v, want DeviceGone", got["bbbb-2222"])
	}
	if got["bbbb-2222"].Reason != "DeviceNotRegistered" {
		t.Fatalf("reason = %q, want the provider error code", got["bbbb-2222"].Reason)
	}
	if got["cccc-3333"].Verdict != notifications.MobilePushRejected {
		t.Fatalf("MessageTooBig receipt = %+v, want Rejected (the device is fine)", got["cccc-3333"])
	}
	if _, ok := got["dddd-4444"]; ok {
		t.Fatal("a ticket Expo said nothing about must be ABSENT from the map, not invented")
	}
}

// The provider's message text can quote the push token back at us. Only the
// machine-readable code may travel further — the same discipline as phone
// masking elsewhere in this repo.
func TestReceiptsNeverCarriesProviderMessageText(t *testing.T) {
	const token = "ExponentPushToken[SECRET-DEVICE-CREDENTIAL]"
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w,
			`{"data":{"t1":{"status":"error","message":"%s is not a registered push token","details":{"error":"DeviceNotRegistered"}}}}`,
			token)
	})
	got, err := s.Receipts(context.Background(), []string{"t1"})
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if strings.Contains(got["t1"].Reason, "ExponentPushToken") {
		t.Fatalf("the receipt reason leaked a push token: %q", got["t1"].Reason)
	}
}

// An error with an unknown (or missing) details.error must not be read as a
// delivery, and must not deactivate anything: Rejected is the safe verdict.
func TestReceiptsUnknownErrorIsRejectedNotDelivered(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"t1":{"status":"error","message":"something new"}}}`)
	})
	got, err := s.Receipts(context.Background(), []string{"t1"})
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if got["t1"].Verdict != notifications.MobilePushRejected {
		t.Fatalf("unknown error = %+v, want Rejected", got["t1"])
	}
	if got["t1"].Reason != "error" {
		t.Fatalf("reason = %q, want the status as a fallback", got["t1"].Reason)
	}
}

// 5xx and 429 are transient: the caller must retry the whole batch, so nothing
// may be reported as decided.
func TestReceiptsTransientStatusIsAnError(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"errors":[{"code":"SERVER_ERROR"}]}`)
			})
			got, err := s.Receipts(context.Background(), []string{"t1"})
			if err == nil {
				t.Fatalf("status %d returned no error (got %v)", code, got)
			}
			if got != nil {
				t.Fatalf("status %d returned decisions: %v", code, got)
			}
		})
	}
}

// A batch above the provider's limit is refused by Expo outright
// (PUSH_TOO_MANY_RECEIPTS). Failing loudly beats silently truncating: a
// truncated batch would drop tickets that are then never polled again.
func TestReceiptsRefusesOversizedBatch(t *testing.T) {
	called := false
	s := newTestSender(t, func(http.ResponseWriter, *http.Request) { called = true })
	ids := make([]string, maxReceiptIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%d", i)
	}
	if _, err := s.Receipts(context.Background(), ids); err == nil {
		t.Fatal("an oversized batch was accepted")
	}
	if called {
		t.Fatal("an oversized batch was still sent to the provider")
	}
}

// An empty batch makes no request at all.
func TestReceiptsEmptyBatchMakesNoRequest(t *testing.T) {
	called := false
	s := newTestSender(t, func(http.ResponseWriter, *http.Request) { called = true })
	got, err := s.Receipts(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty batch = (%v, %v), want an empty map and no error", got, err)
	}
	if called {
		t.Fatal("empty batch still hit the provider")
	}
}

// A 200 that carries only errors[] is Expo refusing the whole request; it must
// not be read as "no receipts yet".
func TestReceiptsRefusalIsAnError(t *testing.T) {
	s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{},"errors":[{"code":"PUSH_TOO_MANY_RECEIPTS","message":"too many"}]}`)
	})
	if _, err := s.Receipts(context.Background(), []string{"t1"}); err == nil {
		t.Fatal("a refused request was reported as success")
	}
}
