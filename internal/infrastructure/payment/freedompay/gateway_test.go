package freedompay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	testMerchantID = "123234"
	testSecretKey  = "freedompay-secret-key-do-not-log"
)

type recorded struct {
	Path   string
	Params url.Values
}

type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server
	// mu guards requests: httptest serves each call on its own goroutine, and a
	// retry test has two in flight at once.
	mu       sync.Mutex
	requests []recorded
	handler  func(path string, attempt int, params url.Values, w http.ResponseWriter)
	counts   map[string]*int32
}

func newFakeGateway(t *testing.T, handler func(path string, attempt int, params url.Values, w http.ResponseWriter)) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t, handler: handler, counts: map[string]*int32{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		params, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		f.requests = append(f.requests, recorded{Path: r.URL.Path, Params: params})
		if f.counts[r.URL.Path] == nil {
			var n int32
			f.counts[r.URL.Path] = &n
		}
		counter := f.counts[r.URL.Path]
		f.mu.Unlock()
		attempt := int(atomic.AddInt32(counter, 1))

		w.Header().Set("Content-Type", "application/xml")
		f.handler(r.URL.Path, attempt, params, w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGateway) gateway(t *testing.T, log *slog.Logger) *Gateway {
	t.Helper()
	client := payment.NewClient(f.srv.Client(), payment.Config{MaxAttempts: 3, Timeout: 100 * time.Millisecond}, log,
		payment.WithSleep(func(ctx context.Context, d time.Duration) error { return ctx.Err() }),
	)
	g, err := New(Config{
		BaseURL:          f.srv.URL,
		MerchantID:       testMerchantID,
		SecretKey:        testSecretKey,
		ResultScriptName: DefaultResultScriptName,
	}, client, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.now = func() time.Time { return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC) }
	return g
}

// respond writes a correctly signed XML answer for the given script.
func respond(w http.ResponseWriter, script string, fields map[string]string) {
	params := url.Values{}
	for k, v := range fields {
		params.Set(k, v)
	}
	params.Set(saltParam, newSalt())
	params.Set(sigParam, sign(script, params, testSecretKey))
	writeXML(w, params)
}

// respondUnsigned writes an answer with a WRONG signature.
func respondUnsigned(w http.ResponseWriter, fields map[string]string) {
	params := url.Values{}
	for k, v := range fields {
		params.Set(k, v)
	}
	params.Set(saltParam, newSalt())
	params.Set(sigParam, "00000000000000000000000000000000")
	writeXML(w, params)
}

func writeXML(w http.ResponseWriter, params url.Values) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><response>`)
	for k, vs := range params {
		for _, v := range vs {
			b.WriteString("<" + k + ">" + v + "</" + k + ">")
		}
	}
	b.WriteString(`</response>`)
	_, _ = io.WriteString(w, b.String())
}

func authorizeRequest() domain.AuthorizeRequest {
	return domain.AuthorizeRequest{
		PaymentID:      uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		BookingID:      uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		IdempotencyKey: "idem-key-1",
		Amount:         domain.KZT(1035000),
		Purpose:        domain.PurposeDeposit,
		Description:    "Депозит и сервисный сбор BookEat",
		ReturnURL:      "https://bookeat.kz/pay/return",
		CallbackURL:    "https://bookeat.kz/webhooks/payments/freedompay",
		CustomerPhone:  "+77010000000",
		CustomerEmail:  "guest@example.com",
	}
}

// ---------------------------------------------------------------------------

func TestAuthorizeInitialisesATwoStagePayment(t *testing.T) {
	f := newFakeGateway(t, func(path string, _ int, _ url.Values, w http.ResponseWriter) {
		if path != pathInitPayment {
			t.Errorf("unexpected path %s", path)
		}
		respond(w, "init_payment", map[string]string{
			"pg_status":       "ok",
			"pg_payment_id":   "1427057029",
			"pg_redirect_url": "https://api.freedompay.kz/pay/1427057029",
		})
	})
	g := f.gateway(t, nil)

	req := authorizeRequest()
	got, err := g.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.ProviderPaymentID != "1427057029" {
		t.Errorf("provider payment id = %q", got.ProviderPaymentID)
	}
	if got.PaymentURL != "https://api.freedompay.kz/pay/1427057029" {
		t.Errorf("payment url = %q", got.PaymentURL)
	}
	if got.Status != domain.PaymentCreated {
		t.Errorf("status = %s, want created", got.Status)
	}

	sent := f.seen()[0].Params
	if sent.Get("pg_order_id") != req.PaymentID.String() {
		t.Errorf("pg_order_id = %q, want our payment id", sent.Get("pg_order_id"))
	}
	if sent.Get("pg_amount") != "10350.00" {
		t.Errorf("pg_amount = %q, want 10350.00", sent.Get("pg_amount"))
	}
	if sent.Get("pg_currency") != "KZT" {
		t.Errorf("pg_currency = %q", sent.Get("pg_currency"))
	}
	if sent.Get("pg_auto_clearing") != "0" {
		t.Errorf("pg_auto_clearing = %q, want 0 (two-stage)", sent.Get("pg_auto_clearing"))
	}
	if sent.Get("pg_idempotency_key") != req.IdempotencyKey {
		t.Errorf("pg_idempotency_key = %q, want our key", sent.Get("pg_idempotency_key"))
	}
	if sent.Get("pg_result_url") != req.CallbackURL {
		t.Errorf("pg_result_url = %q", sent.Get("pg_result_url"))
	}
	if sent.Get("pg_merchant_id") != testMerchantID {
		t.Errorf("pg_merchant_id = %q", sent.Get("pg_merchant_id"))
	}
	// Our own request must be correctly signed, or FreedomPay rejects it with
	// pg_error_code 9998.
	if !verify("init_payment", sent, testSecretKey) {
		t.Error("the outgoing request is not correctly signed")
	}
	if sent.Get(saltParam) == "" {
		t.Error("pg_salt is mandatory in every signed message")
	}
}

func TestAuthorizeRejectsReservedMetadataKeys(t *testing.T) {
	f := newFakeGateway(t, func(string, int, url.Values, http.ResponseWriter) {
		t.Error("the gateway must not be called")
	})
	g := f.gateway(t, nil)

	req := authorizeRequest()
	req.Metadata = map[string]string{"pg_amount": "1"}
	if _, err := g.Authorize(context.Background(), req); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestAuthorizePassesMerchantMetadataThrough(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "init_payment", map[string]string{
			"pg_status": "ok", "pg_payment_id": "1", "pg_redirect_url": "https://x",
		})
	})
	g := f.gateway(t, nil)

	req := authorizeRequest()
	req.Metadata = map[string]string{"restaurant_id": "r-1"}
	if _, err := g.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	sent := f.seen()[0].Params
	if sent.Get("restaurant_id") != "r-1" {
		t.Errorf("metadata not forwarded: %v", sent)
	}
	if sent.Get("booking_id") != req.BookingID.String() {
		t.Errorf("booking_id not forwarded")
	}
	if !verify("init_payment", sent, testSecretKey) {
		t.Error("metadata must participate in the signature")
	}
}

func TestAuthorizeRejectedByGateway(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "init_payment", map[string]string{
			"pg_status":            "error",
			"pg_error_code":        "555",
			"pg_error_description": "Wrong amount",
		})
	})
	g := f.gateway(t, nil)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if !errors.Is(err, payment.ErrProviderRejected) {
		t.Fatalf("err = %v, want ErrProviderRejected", err)
	}
}

// An answer we cannot authenticate is not an answer. Acting on "your refund
// succeeded" from an unverified source is the same mistake as acting on an
// unsigned webhook.
func TestResponseSignatureIsVerified(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respondUnsigned(w, map[string]string{
			"pg_status": "ok", "pg_payment_id": "1", "pg_redirect_url": "https://x",
		})
	})
	g := f.gateway(t, nil)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if !errors.Is(err, payment.ErrProviderMalformed) {
		t.Fatalf("err = %v, want ErrProviderMalformed", err)
	}
}

func TestUnidentifiedMerchantIsAHardFailure(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		// pg_error_code 9998: FreedomPay could not identify us, so the answer
		// is legitimately unsigned.
		params := url.Values{}
		params.Set("pg_status", "error")
		params.Set("pg_error_code", "9998")
		writeXML(w, params)
	})
	g := f.gateway(t, nil)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if !errors.Is(err, payment.ErrProviderRejected) {
		t.Fatalf("err = %v, want ErrProviderRejected", err)
	}
}

func TestAuthorizeRetriesOnTimeout(t *testing.T) {
	f := newFakeGateway(t, func(_ string, attempt int, _ url.Values, w http.ResponseWriter) {
		if attempt == 1 {
			time.Sleep(200 * time.Millisecond)
			return
		}
		respond(w, "init_payment", map[string]string{
			"pg_status": "ok", "pg_payment_id": "1", "pg_redirect_url": "https://x",
		})
	})
	g := f.gateway(t, nil)

	got, err := g.Authorize(context.Background(), authorizeRequest())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.ProviderPaymentID != "1" {
		t.Errorf("provider payment id = %q", got.ProviderPaymentID)
	}
	if len(f.seen()) < 2 {
		t.Fatalf("expected a retry, got %d requests", len(f.seen()))
	}
	// Every attempt must carry the SAME pg_idempotency_key, or a retry could
	// create a second payment.
	for i, r := range f.seen() {
		if r.Params.Get("pg_idempotency_key") != "idem-key-1" {
			t.Errorf("attempt %d carried pg_idempotency_key %q", i, r.Params.Get("pg_idempotency_key"))
		}
	}
}

func TestAuthorizeGivesUpAndReportsUnavailable(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	})
	g := f.gateway(t, nil)

	if _, err := g.Authorize(context.Background(), authorizeRequest()); !errors.Is(err, payment.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(f.seen()) != 3 {
		t.Errorf("attempts = %d, want 3", len(f.seen()))
	}
}

func TestCaptureClearsTheHold(t *testing.T) {
	f := newFakeGateway(t, func(path string, _ int, _ url.Values, w http.ResponseWriter) {
		if path != pathClearing {
			t.Errorf("unexpected path %s", path)
		}
		respond(w, "clearing", map[string]string{
			"pg_status":          "ok",
			"pg_payment_id":      "7777777777",
			"pg_amount":          "10350.00",
			"pg_clearing_amount": "10350.00",
			"pg_status_clearing": "1",
		})
	})
	g := f.gateway(t, nil)

	got, err := g.Capture(context.Background(), "7777777777", domain.KZT(1035000))
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Errorf("status = %s", got.Status)
	}
	if got.Amount != domain.KZT(1035000) {
		t.Errorf("amount = %v", got.Amount)
	}
	if got.CapturedAt == nil {
		t.Error("CapturedAt must be set")
	}
	sent := f.seen()[0].Params
	if sent.Get("pg_payment_id") != "7777777777" || sent.Get("pg_amount") != "10350.00" {
		t.Errorf("sent %v", sent)
	}
	if !verify("clearing", sent, testSecretKey) {
		t.Error("clearing request is not correctly signed")
	}
}

// A partial clearing (a pre-ordered dish the venue could not serve) must report
// what the gateway actually took, not what we asked for.
func TestCapturePartialUsesTheGatewaysClearingAmount(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "clearing", map[string]string{
			"pg_status": "ok", "pg_payment_id": "7777777777",
			"pg_amount": "10350.00", "pg_clearing_amount": "5000.00",
		})
	})
	g := f.gateway(t, nil)

	got, err := g.Capture(context.Background(), "7777777777", domain.KZT(500000))
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Amount != domain.KZT(500000) {
		t.Errorf("amount = %v, want 5000.00 KZT", got.Amount)
	}
}

func TestVoidReleasesTheHold(t *testing.T) {
	f := newFakeGateway(t, func(path string, _ int, _ url.Values, w http.ResponseWriter) {
		if path != pathCancel {
			t.Errorf("unexpected path %s", path)
		}
		respond(w, "cancel", map[string]string{
			"pg_status":            "ok",
			"pg_payment_id":        "7777777777",
			"pg_payment_revoke_id": "7777777778",
			"pg_revoke_status":     "success",
		})
	})
	g := f.gateway(t, nil)

	if err := g.Void(context.Background(), "7777777777"); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if !verify("cancel", f.seen()[0].Params, testSecretKey) {
		t.Error("cancel request is not correctly signed")
	}
}

func TestVoidReportsARefusedRevoke(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "cancel", map[string]string{
			"pg_status": "ok", "pg_payment_id": "7777777777", "pg_revoke_status": "failed",
		})
	})
	g := f.gateway(t, nil)

	if err := g.Void(context.Background(), "7777777777"); !errors.Is(err, payment.ErrProviderRejected) {
		t.Fatalf("err = %v, want ErrProviderRejected", err)
	}
}

func TestRefundFullAndPartial(t *testing.T) {
	tests := []struct {
		name   string
		amount domain.Money
		want   string
	}{
		{"full", domain.KZT(1035000), "10350.00"},
		{"partial", domain.KZT(350), "3.50"},
		{"one tiyn", domain.KZT(1), "0.01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGateway(t, func(path string, _ int, _ url.Values, w http.ResponseWriter) {
				if path != pathRefund {
					t.Errorf("unexpected path %s", path)
				}
				respond(w, "refund", map[string]string{
					"pg_status":            "ok",
					"pg_payment_id":        "7777777777",
					"pg_payment_refund_id": "7777777778",
					"pg_refund_status":     "success",
				})
			})
			g := f.gateway(t, nil)

			got, err := g.Refund(context.Background(), "7777777777", tc.amount)
			if err != nil {
				t.Fatalf("Refund: %v", err)
			}
			if got.ProviderRefundID != "7777777778" {
				t.Errorf("refund id = %q", got.ProviderRefundID)
			}
			if got.Status != domain.RefundSucceeded {
				t.Errorf("status = %s", got.Status)
			}
			if got.Amount != tc.amount {
				t.Errorf("amount = %v", got.Amount)
			}
			if sent := f.seen()[0].Params.Get("pg_amount"); sent != tc.want {
				t.Errorf("pg_amount = %q, want %q", sent, tc.want)
			}
		})
	}
}

func TestGetTranslatesTheGatewayView(t *testing.T) {
	tests := []struct {
		name       string
		fields     map[string]string
		wantStatus domain.PaymentStatus
		wantAmount domain.Money
	}{
		{
			name: "hold, not cleared",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "0",
				"pg_amount": "1030.00", "pg_currency": "KZT", "pg_payment_id": "1427057029",
				"pg_refund_amount": "0", "pg_payment_date": "2024-11-28 15:44:53",
			},
			wantStatus: domain.PaymentAuthorized,
			wantAmount: domain.KZT(103000),
		},
		{
			name: "cleared",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "1",
				"pg_amount": "1030.00", "pg_currency": "KZT", "pg_payment_id": "1427057029",
				"pg_refund_amount": "0",
			},
			wantStatus: domain.PaymentCaptured,
			wantAmount: domain.KZT(103000),
		},
		{
			name: "partially refunded is derived from the money, not the word",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "1",
				"pg_amount": "1030.00", "pg_refund_amount": "500.00", "pg_currency": "KZT",
				"pg_payment_id": "1",
			},
			wantStatus: domain.PaymentPartiallyRefunded,
			wantAmount: domain.KZT(103000),
		},
		{
			// Sandbox 2026-07-22, payment 1814868833: after a partial refund
			// status_v2 returns the refunded sum NEGATIVE, from the merchant's
			// point of view. It must still count as a refund.
			name: "refund reported as a negative amount",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "1",
				"pg_amount": "100", "pg_refund_amount": "-40", "pg_currency": "KZT",
				"pg_payment_id": "1814868833",
			},
			wantStatus: domain.PaymentPartiallyRefunded,
			wantAmount: domain.KZT(10000),
		},
		{
			name: "fully refunded",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "1",
				"pg_amount": "1030.00", "pg_refund_amount": "1030.00", "pg_currency": "KZT",
				"pg_payment_id": "1",
			},
			wantStatus: domain.PaymentRefunded,
			wantAmount: domain.KZT(103000),
		},
		{
			name: "an unknown status word is never read as paid",
			fields: map[string]string{
				"pg_status": "ok", "pg_payment_status": "something_new", "pg_captured": "1",
				"pg_amount": "1030.00", "pg_currency": "KZT", "pg_payment_id": "1",
			},
			wantStatus: domain.PaymentCreated,
			wantAmount: domain.KZT(103000),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGateway(t, func(path string, _ int, _ url.Values, w http.ResponseWriter) {
				if path != pathStatus {
					t.Errorf("unexpected path %s", path)
				}
				respond(w, "status_v2", tc.fields)
			})
			g := f.gateway(t, nil)

			got, err := g.Get(context.Background(), "1427057029")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if got.Amount != tc.wantAmount {
				t.Errorf("amount = %v, want %v", got.Amount, tc.wantAmount)
			}
		})
	}
}

func TestFindByMerchantPaymentIDUsesOrderID(t *testing.T) {
	f := newFakeGateway(t, func(path string, _ int, params url.Values, w http.ResponseWriter) {
		if path != pathStatus {
			t.Errorf("unexpected path %s", path)
		}
		if params.Get("pg_order_id") != "our-payment-id" {
			t.Errorf("pg_order_id = %q", params.Get("pg_order_id"))
		}
		if params.Get("pg_payment_id") != "" {
			t.Error("pg_payment_id must not be sent when looking up by our own id")
		}
		respond(w, "status_v2", map[string]string{
			"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "0",
			"pg_amount": "10.00", "pg_currency": "KZT", "pg_payment_id": "42",
		})
	})
	g := f.gateway(t, nil)

	got, err := g.FindByMerchantPaymentID(context.Background(), "our-payment-id")
	if err != nil {
		t.Fatalf("FindByMerchantPaymentID: %v", err)
	}
	if got.ProviderPaymentID != "42" || got.Status != domain.PaymentAuthorized {
		t.Errorf("got %+v", got)
	}
}

func TestOperationsRejectEmptyIDsAndAmounts(t *testing.T) {
	f := newFakeGateway(t, func(string, int, url.Values, http.ResponseWriter) {
		t.Error("the gateway must not be called")
	})
	g := f.gateway(t, nil)
	ctx := context.Background()

	if _, err := g.Capture(ctx, "", domain.KZT(1)); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Capture: err = %v", err)
	}
	if _, err := g.Capture(ctx, "1", domain.KZT(0)); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Capture zero amount: err = %v", err)
	}
	if err := g.Void(ctx, "  "); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Void: err = %v", err)
	}
	if _, err := g.Refund(ctx, "1", domain.KZT(0)); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Refund: err = %v", err)
	}
	if _, err := g.Get(ctx, ""); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Get: err = %v", err)
	}
	if _, err := g.FindByMerchantPaymentID(ctx, ""); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Find: err = %v", err)
	}
}

func TestMalformedResponse(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `<response><pg_status>ok`)
	})
	g := f.gateway(t, nil)

	if _, err := g.Get(context.Background(), "1"); !errors.Is(err, payment.ErrProviderMalformed) {
		t.Fatalf("err = %v, want ErrProviderMalformed", err)
	}
}

// The secret key is the signing key: it must never reach an error string or a
// log line, and neither must the signature computed from it.
func TestErrorsAndLogsCarryNoSecrets(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<response><pg_error_description>bad key `+testSecretKey+`</pg_error_description></response>`)
	})

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g := f.gateway(t, log)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testSecretKey) {
		t.Errorf("error leaks the secret key: %q", err)
	}
	if strings.Contains(logs.String(), testSecretKey) {
		t.Errorf("log leaks the secret key: %s", logs.String())
	}
	// The signature we sent must not be logged either.
	if len(f.seen()) > 0 {
		if sig := f.seen()[0].Params.Get(sigParam); sig != "" && strings.Contains(logs.String(), sig) {
			t.Errorf("log leaks pg_sig: %s", logs.String())
		}
	}
}

// Card data and pg_sig must not reach the audit payload we store in
// payment_events (spec §8).
func TestStoredPayloadRedactsCardDataAndSignature(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "status_v2", map[string]string{
			"pg_status": "ok", "pg_payment_status": "success", "pg_captured": "1",
			"pg_amount": "10.00", "pg_currency": "KZT", "pg_payment_id": "1",
			"pg_card_pan": "4400-44XX-XXXX-4444", "pg_card_exp": "12/24",
			"pg_card_name": "NAME NAME",
		})
	})
	g := f.gateway(t, nil)

	got, err := g.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, forbidden := range []string{"4400-44XX-XXXX-4444", "12/24", "NAME NAME"} {
		if strings.Contains(string(got.Raw), forbidden) {
			t.Errorf("stored payload leaks %q: %s", forbidden, got.Raw)
		}
	}
	if !strings.Contains(string(got.Raw), `"pg_sig":"[redacted]"`) {
		t.Errorf("pg_sig must be redacted: %s", got.Raw)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{BaseURL: DefaultBaseURL, MerchantID: "1", SecretKey: "s", ResultScriptName: "freedompay"}, false},
		{"no merchant id", Config{BaseURL: DefaultBaseURL, SecretKey: "s", ResultScriptName: "freedompay"}, true},
		{"no secret", Config{BaseURL: DefaultBaseURL, MerchantID: "1", ResultScriptName: "freedompay"}, true},
		{"no base url", Config{MerchantID: "1", SecretKey: "s", ResultScriptName: "freedompay"}, true},
		{"no result script name", Config{BaseURL: DefaultBaseURL, MerchantID: "1", SecretKey: "s"}, true},
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

func TestName(t *testing.T) {
	g, err := New(Config{BaseURL: DefaultBaseURL, MerchantID: "1", SecretKey: "s", ResultScriptName: "freedompay"}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if g.Name() != domain.ProviderFreedomPay {
		t.Errorf("Name() = %s", g.Name())
	}
}

// seen returns a snapshot of the requests the fake gateway received.
func (f *fakeGateway) seen() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}
