package tiptoppay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

const (
	testPublicID  = "pk_test_public_id"
	testAPISecret = "tiptop-api-secret-do-not-log"
)

// recorded is one request the fake acquirer saw.
type recorded struct {
	Path      string
	RequestID string
	Auth      string
	Body      map[string]any
}

// fakeAcquirer is an httptest server impersonating TipTopPay.
type fakeAcquirer struct {
	t   *testing.T
	srv *httptest.Server
	// mu guards requests: httptest serves each call on its own goroutine, and a
	// retry test has two in flight at once.
	mu       sync.Mutex
	requests []recorded
	// handler answers a path; the int is the attempt number for that path.
	handler func(path string, attempt int, body map[string]any, w http.ResponseWriter)
	counts  map[string]*int32
}

func newFakeAcquirer(t *testing.T, handler func(path string, attempt int, body map[string]any, w http.ResponseWriter)) *fakeAcquirer {
	t.Helper()
	f := &fakeAcquirer{t: t, handler: handler, counts: map[string]*int32{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		f.mu.Lock()
		f.requests = append(f.requests, recorded{
			Path:      r.URL.Path,
			RequestID: r.Header.Get("X-Request-ID"),
			Auth:      r.Header.Get("Authorization"),
			Body:      body,
		})
		if f.counts[r.URL.Path] == nil {
			var n int32
			f.counts[r.URL.Path] = &n
		}
		counter := f.counts[r.URL.Path]
		f.mu.Unlock()
		attempt := int(atomic.AddInt32(counter, 1))

		w.Header().Set("Content-Type", "application/json")
		f.handler(r.URL.Path, attempt, body, w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAcquirer) gateway(t *testing.T, log *slog.Logger) *Gateway {
	t.Helper()
	client := payment.NewClient(f.srv.Client(), payment.Config{MaxAttempts: 3, Timeout: 100 * time.Millisecond}, log,
		payment.WithSleep(func(ctx context.Context, d time.Duration) error { return ctx.Err() }),
	)
	g, err := New(Config{BaseURL: f.srv.URL, PublicID: testPublicID, APISecret: testAPISecret}, client, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.now = func() time.Time { return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC) }
	return g
}

func ok(w http.ResponseWriter, model string) {
	if model == "" {
		_, _ = io.WriteString(w, `{"Success":true,"Message":null}`)
		return
	}
	_, _ = io.WriteString(w, `{"Model":`+model+`,"Success":true,"Message":null}`)
}

func rejected(w http.ResponseWriter, message string) {
	_, _ = io.WriteString(w, `{"Model":null,"Success":false,"Message":"`+message+`"}`)
}

func authorizeRequest() domain.AuthorizeRequest {
	return domain.AuthorizeRequest{
		PaymentID:      uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		BookingID:      uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		IdempotencyKey: "idem-key-1",
		Amount:         domain.KZT(1035000),
		Purpose:        domain.PurposeDeposit,
		Description:    "Сервисный сбор BookEat и депозит",
		ReturnURL:      "https://bookeat.kz/pay/return",
		CustomerPhone:  "+77010000000",
		CustomerEmail:  "guest@example.com",
	}
}

// ---------------------------------------------------------------------------

func TestAuthorizeCreatesATwoStageOrder(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/orders/create" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, `{"Id":"gASGZVgUN21hcpPF","Number":2130,"Currency":"KZT","Url":"https://orders.tiptoppay.kz/d/gASGZVgUN21hcpPF","Status":"Created","StatusCode":0}`)
	})
	g := f.gateway(t, nil)

	req := authorizeRequest()
	got, err := g.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if got.ProviderPaymentID != "gASGZVgUN21hcpPF" {
		t.Errorf("provider payment id = %q", got.ProviderPaymentID)
	}
	if got.PaymentURL != "https://orders.tiptoppay.kz/d/gASGZVgUN21hcpPF" {
		t.Errorf("payment url = %q", got.PaymentURL)
	}
	// Nobody has paid yet: an order is an invitation, not a hold.
	if got.Status != domain.PaymentCreated {
		t.Errorf("status = %s, want created", got.Status)
	}
	if got.Amount != req.Amount {
		t.Errorf("amount = %v, want %v", got.Amount, req.Amount)
	}

	sent := f.seen()[0]
	if sent.RequestID != req.IdempotencyKey {
		t.Errorf("X-Request-ID = %q, want our idempotency key", sent.RequestID)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testPublicID+":"+testAPISecret))
	if sent.Auth != wantAuth {
		t.Errorf("authorization header not sent as HTTP Basic")
	}
	if sent.Body["RequireConfirmation"] != true {
		t.Error("RequireConfirmation must be true — the two-stage flow is mandatory")
	}
	// Money must travel as an exact decimal, never as a float literal.
	if sent.Body["Amount"] != 10350.0 {
		t.Errorf("Amount = %v, want 10350.00", sent.Body["Amount"])
	}
	if sent.Body["InvoiceId"] != req.PaymentID.String() {
		t.Errorf("InvoiceId = %v, want our payment id", sent.Body["InvoiceId"])
	}
}

func TestAuthorizeRejectsInvalidRequests(t *testing.T) {
	f := newFakeAcquirer(t, func(string, int, map[string]any, http.ResponseWriter) {
		t.Error("the acquirer must not be called for an invalid request")
	})
	g := f.gateway(t, nil)

	tests := []struct {
		name  string
		mutar func(*domain.AuthorizeRequest)
	}{
		{"no payment id", func(r *domain.AuthorizeRequest) { r.PaymentID = uuid.Nil }},
		{"no idempotency key", func(r *domain.AuthorizeRequest) { r.IdempotencyKey = "" }},
		{"zero amount", func(r *domain.AuthorizeRequest) { r.Amount = domain.KZT(0) }},
		{"unsupported currency", func(r *domain.AuthorizeRequest) { r.Amount = domain.Money{AmountMinor: 100, Currency: "USD"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := authorizeRequest()
			tc.mutar(&req)
			_, err := g.Authorize(context.Background(), req)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want domain.ErrValidation", err)
			}
		})
	}
}

func TestAuthorizeDeclinedByAcquirer(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		rejected(w, "Amount is invalid")
	})
	g := f.gateway(t, nil)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if !errors.Is(err, payment.ErrProviderRejected) {
		t.Fatalf("err = %v, want ErrProviderRejected", err)
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want it to wrap domain.ErrValidation", err)
	}
}

// A timeout followed by a retry must reach the acquirer with the SAME
// X-Request-ID, which is what makes a second money movement impossible.
func TestAuthorizeRetriesWithTheSameIdempotencyKey(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, attempt int, _ map[string]any, w http.ResponseWriter) {
		if attempt == 1 {
			time.Sleep(200 * time.Millisecond) // outlast the 100ms per-attempt budget
			return
		}
		ok(w, `{"Id":"order-1","Url":"https://orders.tiptoppay.kz/d/order-1"}`)
	})
	g := f.gateway(t, nil)

	got, err := g.Authorize(context.Background(), authorizeRequest())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.ProviderPaymentID != "order-1" {
		t.Errorf("provider payment id = %q", got.ProviderPaymentID)
	}
	if len(f.seen()) < 2 {
		t.Fatalf("expected a retry, got %d requests", len(f.seen()))
	}
	for i, r := range f.seen() {
		if r.RequestID != "idem-key-1" {
			t.Errorf("request %d carried X-Request-ID %q, want the original key", i, r.RequestID)
		}
	}
}

func TestAuthorizeGivesUpAndReportsUnavailable(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	})
	g := f.gateway(t, nil)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if !errors.Is(err, payment.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(f.seen()) != 3 {
		t.Errorf("attempts = %d, want 3", len(f.seen()))
	}
}

func TestCaptureConfirmsTheHold(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/payments/confirm" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, "")
	})
	g := f.gateway(t, nil)

	got, err := g.Capture(context.Background(), "897749645", domain.KZT(1035000))
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Errorf("status = %s, want captured", got.Status)
	}
	if got.CapturedAt == nil {
		t.Error("CapturedAt must be set")
	}
	body := f.seen()[0].Body
	if body["TransactionId"] != 897749645.0 {
		t.Errorf("TransactionId = %v", body["TransactionId"])
	}
	if body["Amount"] != 10350.0 {
		t.Errorf("Amount = %v, want 10350.00", body["Amount"])
	}
	if f.seen()[0].RequestID == "" {
		t.Error("capture must carry a derived X-Request-ID so a retry is a replay")
	}
}

// The derived idempotency key must be a pure function of the operation, so a
// repeat of the same capture is deduplicated by the acquirer.
func TestDerivedIdempotencyKeyIsStableAndOperationScoped(t *testing.T) {
	a := derivedKey("confirm", "123", domain.KZT(500))
	b := derivedKey("confirm", "123", domain.KZT(500))
	if a != b {
		t.Error("the same capture must derive the same key")
	}
	if derivedKey("confirm", "123", domain.KZT(501)) == a {
		t.Error("a different amount must derive a different key")
	}
	if derivedKey("void", "123", domain.KZT(500)) == a {
		t.Error("a different operation must derive a different key")
	}
	if derivedKey("confirm", "124", domain.KZT(500)) == a {
		t.Error("a different transaction must derive a different key")
	}
}

// Capturing before the guest paid: the stored id is still an ORDER id, and
// there is nothing to confirm. This must fail loudly, not pretend to work.
func TestCaptureVoidRefundGetRejectAnOrderID(t *testing.T) {
	f := newFakeAcquirer(t, func(string, int, map[string]any, http.ResponseWriter) {
		t.Error("the acquirer must not be called with an order id")
	})
	g := f.gateway(t, nil)
	ctx := context.Background()

	if _, err := g.Capture(ctx, "gASGZVgUN21hcpPF", domain.KZT(100)); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Errorf("Capture: err = %v, want ErrInvalidStatus", err)
	}
	if err := g.Void(ctx, "gASGZVgUN21hcpPF"); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Errorf("Void: err = %v, want ErrInvalidStatus", err)
	}
	if _, err := g.Refund(ctx, "gASGZVgUN21hcpPF", domain.KZT(100)); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Errorf("Refund: err = %v, want ErrInvalidStatus", err)
	}
	if _, err := g.Get(ctx, "gASGZVgUN21hcpPF"); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Errorf("Get: err = %v, want ErrInvalidStatus", err)
	}
}

func TestVoidReleasesTheHold(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/payments/void" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, "")
	})
	g := f.gateway(t, nil)

	if err := g.Void(context.Background(), "455"); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if f.seen()[0].Body["TransactionId"] != 455.0 {
		t.Errorf("TransactionId = %v", f.seen()[0].Body["TransactionId"])
	}
}

func TestRefundFullAndPartial(t *testing.T) {
	tests := []struct {
		name       string
		amount     domain.Money
		wantAmount float64
	}{
		{"full", domain.KZT(1035000), 10350},
		{"partial", domain.KZT(500000), 5000},
		{"one tiyn", domain.KZT(1), 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
				if path != "/payments/refund" {
					t.Errorf("unexpected path %s", path)
				}
				ok(w, `{"TransactionId":568}`)
			})
			g := f.gateway(t, nil)

			got, err := g.Refund(context.Background(), "455", tc.amount)
			if err != nil {
				t.Fatalf("Refund: %v", err)
			}
			if got.ProviderRefundID != "568" {
				t.Errorf("refund id = %q", got.ProviderRefundID)
			}
			if got.Status != domain.RefundSucceeded {
				t.Errorf("status = %s", got.Status)
			}
			if got.Amount != tc.amount {
				t.Errorf("amount = %v, want %v", got.Amount, tc.amount)
			}
			if f.seen()[0].Body["Amount"] != tc.wantAmount {
				t.Errorf("sent Amount = %v, want %v", f.seen()[0].Body["Amount"], tc.wantAmount)
			}
		})
	}
}

func TestRefundRejectsNonPositiveAmounts(t *testing.T) {
	f := newFakeAcquirer(t, func(string, int, map[string]any, http.ResponseWriter) {
		t.Error("the acquirer must not be called")
	})
	g := f.gateway(t, nil)
	if _, err := g.Refund(context.Background(), "455", domain.KZT(0)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestGetTranslatesTheAcquirerView(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/payments/get" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, `{"TransactionId":897749645,"Amount":159,"Currency":"KZT","InvoiceId":"12345",
		        "AuthDateIso":"2021-10-30T04:00:02","ConfirmDateIso":"2021-10-30T04:00:06",
		        "Status":"Completed","StatusCode":3,"Reason":"Approved","ReasonCode":0,
		        "CardFirstSix":"424242","CardLastFour":"4242","Token":"tok"}`)
	})
	g := f.gateway(t, nil)

	got, err := g.Get(context.Background(), "897749645")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Errorf("status = %s, want captured", got.Status)
	}
	if got.Amount != domain.KZT(15900) {
		t.Errorf("amount = %v, want 159.00 KZT", got.Amount)
	}
	if got.AuthorizedAt == nil || !got.AuthorizedAt.Equal(time.Date(2021, 10, 30, 4, 0, 2, 0, time.UTC)) {
		t.Errorf("AuthorizedAt = %v", got.AuthorizedAt)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(time.Date(2021, 10, 30, 4, 0, 6, 0, time.UTC)) {
		t.Errorf("CapturedAt = %v", got.CapturedAt)
	}
	// Card identifiers are not decoded, so they cannot leak from the mapped
	// struct. (They remain in Raw, which the caller redacts before storing.)
	if strings.Contains(got.FailureMessage, "4242") {
		t.Error("card data leaked into the mapped payment")
	}
}

func TestFindByMerchantPaymentIDUsesOurOwnID(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, body map[string]any, w http.ResponseWriter) {
		if path != "/v2/payments/find" {
			t.Errorf("unexpected path %s", path)
		}
		if body["InvoiceId"] != "11111111-1111-4111-8111-111111111111" {
			t.Errorf("InvoiceId = %v", body["InvoiceId"])
		}
		ok(w, `{"TransactionId":42,"Amount":159,"Currency":"KZT","Status":"Authorized"}`)
	})
	g := f.gateway(t, nil)

	got, err := g.FindByMerchantPaymentID(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("FindByMerchantPaymentID: %v", err)
	}
	if got.ProviderPaymentID != "42" || got.Status != domain.PaymentAuthorized {
		t.Errorf("got %+v", got)
	}
}

func TestMalformedResponse(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `not json at all`)
	})
	g := f.gateway(t, nil)

	if _, err := g.Get(context.Background(), "1"); !errors.Is(err, payment.ErrProviderMalformed) {
		t.Fatalf("err = %v, want ErrProviderMalformed", err)
	}
}

// A 401 body may repeat the credentials we sent; none of it may reach the
// error, and neither may the configured secret.
func TestErrorsAndLogsCarryNoSecrets(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"Message":"bad credentials `+testAPISecret+`","Success":false}`)
	})

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g := f.gateway(t, log)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testAPISecret) {
		t.Errorf("error leaks the API secret: %q", err)
	}
	if strings.Contains(err.Error(), testPublicID) {
		t.Errorf("error leaks the public id: %q", err)
	}
	if strings.Contains(logs.String(), testAPISecret) {
		t.Errorf("log leaks the API secret: %s", logs.String())
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{BaseURL: DefaultBaseURL, PublicID: "pk", APISecret: "s"}, false},
		{"no public id", Config{BaseURL: DefaultBaseURL, APISecret: "s"}, true},
		{"no secret", Config{BaseURL: DefaultBaseURL, PublicID: "pk"}, true},
		{"no base url", Config{PublicID: "pk", APISecret: "s"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	if _, err := New(Config{}, nil, nil); err == nil {
		t.Error("New must refuse an incomplete configuration")
	}
}

// seen returns a snapshot of the requests the fake acquirer received.
func (f *fakeAcquirer) seen() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}
