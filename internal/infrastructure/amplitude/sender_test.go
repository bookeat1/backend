package amplitude

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend-core/internal/usecase/analytics"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sampleBatch() []analytics.Event {
	return []analytics.Event{
		{
			Type:     analytics.EventBookingConfirmed,
			UserID:   "11111111-1111-1111-1111-111111111111",
			DeviceID: "bk-22222222-2222-2222-2222-222222222222",
			InsertID: "33333333-3333-3333-3333-333333333333",
			Time:     time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
			Properties: map[string]any{
				"restaurant_id": "44444444-4444-4444-4444-444444444444",
				"guests":        4,
				"status":        "confirmed",
			},
		},
	}
}

func TestConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("empty api key must not be Configured")
	}
	if (Config{APIKey: "  "}).Configured() {
		t.Fatal("blank api key must not be Configured")
	}
	if !(Config{APIKey: "abc"}).Configured() {
		t.Fatal("a real key must be Configured")
	}
}

// The uploaded payload must have the right shape (api_key + events with the
// documented fields) and NO raw PII.
func TestSend_PayloadShapeAndNoPII(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"events_ingested":1}`))
	}))
	defer srv.Close()

	c := NewClient(Config{APIKey: "secret-key", Endpoint: srv.URL}, testLogger())
	if err := c.Send(context.Background(), sampleBatch()); err != nil {
		t.Fatalf("send: %v", err)
	}

	var req apiRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("server got non-JSON body: %v", err)
	}
	if req.APIKey != "secret-key" {
		t.Fatalf("api_key = %q", req.APIKey)
	}
	if len(req.Events) != 1 {
		t.Fatalf("events len = %d, want 1", len(req.Events))
	}
	e := req.Events[0]
	if e.EventType != "booking_confirmed" {
		t.Fatalf("event_type = %q", e.EventType)
	}
	if e.UserID == "" || e.DeviceID == "" || e.InsertID == "" {
		t.Fatalf("identity fields must be set: %+v", e)
	}
	if e.Time != time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("time = %d, want ms epoch", e.Time)
	}

	// Hard no-PII assertion over the raw wire bytes.
	s := strings.ToLower(string(gotBody))
	for _, needle := range []string{"phone", "email", "\"name\"", "+7701", "@example"} {
		if strings.Contains(s, needle) {
			t.Fatalf("PII marker %q found on the wire: %s", needle, gotBody)
		}
	}
}

func TestSend_EmptyBatchNoCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(Config{APIKey: "k", Endpoint: srv.URL}, testLogger())
	if err := c.Send(context.Background(), nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if called {
		t.Fatal("an empty batch must not hit the network")
	}
}

// 429 and 5xx are transient — Send returns an error so the worker retries.
func TestSend_TransientStatusesError(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := NewClient(Config{APIKey: "k", Endpoint: srv.URL}, testLogger())
		err := c.Send(context.Background(), sampleBatch())
		srv.Close()
		if err == nil {
			t.Fatalf("status %d must return a retryable error", code)
		}
	}
}

// 400/413 are permanent for this batch — Send drops (nil error) so one poison
// batch cannot block the stream forever.
func TestSend_PermanentStatusesDrop(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"code":` + http.StatusText(code) + `}`))
		}))
		c := NewClient(Config{APIKey: "k", Endpoint: srv.URL}, testLogger())
		err := c.Send(context.Background(), sampleBatch())
		srv.Close()
		if err != nil {
			t.Fatalf("status %d must be dropped (nil error), got %v", code, err)
		}
	}
}

func TestNewClient_DefaultEndpoint(t *testing.T) {
	c := NewClient(Config{APIKey: "k"}, testLogger())
	if c.endpoint != DefaultEndpoint {
		t.Fatalf("endpoint = %q, want default %q", c.endpoint, DefaultEndpoint)
	}
}
