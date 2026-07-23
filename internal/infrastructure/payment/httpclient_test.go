package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// testClient builds a Client whose backoff does not actually sleep, so a retry
// test runs in microseconds instead of seconds.
func testClient(t *testing.T, doer Doer, cfg Config, log *slog.Logger) (*Client, *int32) {
	t.Helper()
	var slept int32
	c := NewClient(doer, cfg, log,
		WithSleep(func(ctx context.Context, d time.Duration) error {
			atomic.AddInt32(&slept, 1)
			return ctx.Err()
		}),
		WithJitter(func(d time.Duration) time.Duration { return d }),
	)
	return c, &slept
}

func TestClientRetriesIdempotentCallsOnServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, slept := testClient(t, srv.Client(), Config{MaxAttempts: 3}, nil)
	resp, err := c.Do(context.Background(), Request{
		Op: "probe", Method: http.MethodPost, URL: srv.URL, Idempotent: true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
	if *slept != 2 {
		t.Errorf("backoff pauses = %d, want 2", *slept)
	}
}

// A non-idempotent call must be attempted exactly once. Retrying a request the
// provider cannot deduplicate is how a guest gets charged twice.
func TestClientDoesNotRetryNonIdempotentCalls(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 5}, nil)
	if _, err := c.Do(context.Background(), Request{
		Op: "charge", Method: http.MethodPost, URL: srv.URL, Idempotent: false,
	}); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1", got)
	}
}

func TestClientRetriesOnTimeoutThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Outlast the per-attempt timeout below.
			time.Sleep(150 * time.Millisecond)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, _ := testClient(t, srv.Client(), Config{Timeout: 30 * time.Millisecond, MaxAttempts: 3}, nil)
	resp, err := c.Do(context.Background(), Request{
		Op: "get", Method: http.MethodGet, URL: srv.URL, Idempotent: true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("server calls = %d, want at least 2", got)
	}
}

func TestClientGivesUpAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 2}, nil)
	_, err := c.Do(context.Background(), Request{
		Op: "probe", Method: http.MethodGet, URL: srv.URL, Idempotent: true,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
}

// A 4xx is our fault; retrying it wastes the guest's time and the acquirer's
// rate limit.
func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 4}, nil)
	resp, err := c.Do(context.Background(), Request{
		Op: "probe", Method: http.MethodGet, URL: srv.URL, Idempotent: true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1", got)
	}
}

func TestClientHonoursCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 3}, nil)
	_, err := c.Do(ctx, Request{Op: "probe", Method: http.MethodGet, URL: srv.URL, Idempotent: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// The whole point of the error discipline in this package: whatever goes wrong,
// nothing that could authenticate us ends up in a message someone will paste
// into a ticket.
func TestErrorsCarryNoSecrets(t *testing.T) {
	const secret = "super-secret-api-key-42"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// A hostile or careless provider echoing our credentials back.
		_, _ = w.Write([]byte(`{"Message":"bad key ` + secret + `"}`))
	}))
	defer srv.Close()

	header := http.Header{}
	header.Set("Authorization", "Basic "+secret)

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 2}, nil)
	_, err := c.Do(context.Background(), Request{
		Op:         "probe",
		Method:     http.MethodPost,
		URL:        srv.URL + "?token=" + secret,
		Header:     header,
		Body:       []byte(`{"secret":"` + secret + `"}`),
		Idempotent: true,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the secret: %q", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error leaks the URL (may carry query credentials): %q", err)
	}
}

// Logs are the other place secrets escape to. The shared client logs only the
// provider, the operation and the attempt counters.
func TestLogsCarryNoSecrets(t *testing.T) {
	const secret = "super-secret-api-key-42"

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	header := http.Header{}
	header.Set("Authorization", "Basic "+secret)

	c, _ := testClient(t, srv.Client(), Config{MaxAttempts: 2}, log)
	_, _ = c.Do(context.Background(), Request{
		Provider: domain.ProviderTipTopPay, Op: "probe",
		Method: http.MethodPost, URL: srv.URL, Header: header,
		Body: []byte(`{"CardCryptogramPacket":"` + secret + `"}`), Idempotent: true,
	})

	out := buf.String()
	if out == "" {
		t.Fatal("expected the failed attempts to be logged")
	}
	if strings.Contains(out, secret) {
		t.Fatalf("log leaks the secret: %s", out)
	}
	// Sanity: it did log something useful.
	var line map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&line); err != nil {
		t.Fatalf("log is not JSON: %v", err)
	}
	if line["op"] != "probe" {
		t.Errorf("log op = %v, want probe", line["op"])
	}
}

func TestBackoffIsBoundedAndGrows(t *testing.T) {
	c := NewClient(nil, Config{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond}, nil,
		WithJitter(func(d time.Duration) time.Duration { return d }))

	want := []time.Duration{100, 200, 400, 400, 400}
	for i, w := range want {
		if got := c.backoff(i + 1); got != w*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
}
