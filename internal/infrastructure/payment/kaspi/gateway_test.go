package kaspi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

const (
	testCompany = "7"
	testAPIKey  = "kpk_test_key_not_a_real_credential"
	testSecret  = "test_webhook_secret_not_a_real_credential"
)

// newTestGateway wires the adapter against an httptest server. There is no
// Kaspi sandbox — this is the only place the pipeline can be exercised at all.
func newTestGateway(t *testing.T, h http.HandlerFunc) (*Gateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	client := payment.NewClient(srv.Client(), payment.Config{MaxAttempts: 1}, nil,
		payment.WithSleep(func(context.Context, time.Duration) error { return nil }))
	gw, err := New(Config{
		BaseURL:        srv.URL,
		CompanyAPIKeys: map[string]string{testCompany: testAPIKey},
		WebhookSecrets: map[string]string{testCompany: testSecret},
	}, client, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return gw, srv
}

func authorizeReq(amountMinor int64) domain.AuthorizeRequest {
	return domain.AuthorizeRequest{
		PaymentID:          uuid.New(),
		BookingID:          uuid.New(),
		IdempotencyKey:     "booking:guest:key",
		Amount:             domain.Money{AmountMinor: amountMinor, Currency: domain.CurrencyKZT},
		Purpose:            domain.PurposePreorder,
		MerchantAccountRef: testCompany,
	}
}

func TestAuthorizeReturnsLinkAndTheProvidersOwnExpiry(t *testing.T) {
	expire := time.Now().Add(3 * time.Minute).UTC().Truncate(time.Second)
	var gotBody map[string]any
	var gotPath, gotKey string

	gw, _ := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"Data":{"QrOperationId":991,"QrToken":"https://pay.kaspi.kz/pay/abc",` +
			`"Amount":2539,"ExpireDate":"` + expire.Format(time.RFC3339) + `","Status":"QrTokenCreated"}}`))
	})

	// 2539 ₸ — a whole number of tenge, as the checkout guarantees.
	out, err := gw.Authorize(context.Background(), authorizeReq(253_900))
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if gotPath != "/api/qr/create" {
		t.Fatalf("path = %q, want /api/qr/create", gotPath)
	}
	if gotKey != testAPIKey {
		t.Fatalf("the company's own api key was not sent")
	}
	// The wire amount is whole TENGE, never tiyn: sending 253900 would charge
	// the guest a hundred times over.
	if got := gotBody["amount"]; got != float64(2539) {
		t.Fatalf("amount sent = %v, want 2539 (tenge)", got)
	}
	if out.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, want created — a link is not a payment", out.Status)
	}
	if out.PaymentURL != "https://pay.kaspi.kz/pay/abc" {
		t.Fatalf("payment url = %q", out.PaymentURL)
	}
	if out.ProviderPaymentID != "991" {
		t.Fatalf("provider payment id = %q, want 991", out.ProviderPaymentID)
	}
	if out.ExpiresAt == nil || !out.ExpiresAt.Equal(expire) {
		t.Fatalf("expiry = %v, want the provider's own %v", out.ExpiresAt, expire)
	}
}

func TestAuthorizeRewritesTheQrHostToTheOneAPhoneOpens(t *testing.T) {
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"QrOperationId":"1","QrToken":"https://qr.kaspi.kz/abc","Status":"QrTokenCreated"}}`))
	})
	out, err := gw.Authorize(context.Background(), authorizeReq(100))
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if out.PaymentURL != "https://pay.kaspi.kz/pay/abc" {
		t.Fatalf("payment url = %q, want the pay.kaspi.kz form", out.PaymentURL)
	}
}

func TestAuthorizeRefusesAVenueWithNoKaspiCompany(t *testing.T) {
	called := false
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) { called = true })

	req := authorizeReq(100)
	req.MerchantAccountRef = ""
	_, err := gw.Authorize(context.Background(), req)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeSplitAccountMissing {
		t.Fatalf("machine code = %q/%v, want %q", code, ok, domain.CodeSplitAccountMissing)
	}
	if called {
		t.Fatal("a venue with no company must not reach the acquirer at all")
	}
}

func TestAuthorizeRefusesAnAmountKaspiCannotCharge(t *testing.T) {
	called := false
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) { called = true })

	// 2538.07 ₸ — what a gross-up produces if nobody rounds it.
	_, err := gw.Authorize(context.Background(), authorizeReq(253_807))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation for a sub-tenge amount", err)
	}
	if called {
		t.Fatal("a fractional amount must be refused before the acquirer is called, never rounded")
	}
}

func TestAuthorizeIsNeverRetried(t *testing.T) {
	// /api/qr/create takes no idempotency key: a retry would create a SECOND
	// payable link for one booking.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	client := payment.NewClient(srv.Client(), payment.Config{MaxAttempts: 5}, nil,
		payment.WithSleep(func(context.Context, time.Duration) error { return nil }))
	gw, err := New(Config{
		BaseURL:        srv.URL,
		CompanyAPIKeys: map[string]string{testCompany: testAPIKey},
		WebhookSecrets: map[string]string{testCompany: testSecret},
	}, client, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := gw.Authorize(context.Background(), authorizeReq(100)); err == nil {
		t.Fatal("Authorize() error = nil, want a failure")
	}
	if calls != 1 {
		t.Fatalf("qr/create was attempted %d times, want exactly 1 — a retry creates a second payable link", calls)
	}
}

func TestCaptureConfirmsAPaidLinkAndMovesNoMoney(t *testing.T) {
	var methods []string
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"Data":{"Status":"Processed","Amount":2539}}`))
	})

	out, err := gw.Capture(context.Background(), "991", domain.Money{AmountMinor: 253_900, Currency: domain.CurrencyKZT})
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if out.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", out.Status)
	}
	// The whole point: a Kaspi "capture" is a READ. Anything that POSTs here
	// would be moving money a second time.
	for _, m := range methods {
		if !strings.HasPrefix(m, http.MethodGet+" ") {
			t.Fatalf("capture performed %q — it must only read", m)
		}
	}
}

func TestCaptureOnAnUnpaidLinkIsUnknownNotSuccess(t *testing.T) {
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"Status":"QrTokenCreated"}}`))
	})
	_, err := gw.Capture(context.Background(), "991", domain.Money{AmountMinor: 100, Currency: domain.CurrencyKZT})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrProviderOutcomeUnknown", err)
	}
}

func TestCaptureOnARefusedLinkIsADefiniteDecline(t *testing.T) {
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"Status":"CancelledByUser","StatusDesc":"guest declined"}}`))
	})
	_, err := gw.Capture(context.Background(), "991", domain.Money{AmountMinor: 100, Currency: domain.CurrencyKZT})
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("error = %v, want ErrProviderDeclined so the capture claim is released", err)
	}
}

func TestCaptureRefusesAnAmountThatIsNotWhatWasPaid(t *testing.T) {
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"Status":"Processed","Amount":10}}`))
	})
	_, err := gw.Capture(context.Background(), "991", domain.Money{AmountMinor: 253_900, Currency: domain.CurrencyKZT})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("error = %v, want the mismatch surfaced for reconciliation", err)
	}
}

func TestGetNeverReadsAnUnknownStatusAsPaid(t *testing.T) {
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"Status":"SomethingKaspiInventedLastWeek"}}`))
	})
	got, err := gw.Get(context.Background(), "991")
	if err == nil {
		t.Fatalf("Get() = %+v, want an error on an unknown status", got)
	}
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrProviderOutcomeUnknown", err)
	}
}

func TestVoidIsANoOpOnAnUnpaidLinkAndRefusesAPaidOne(t *testing.T) {
	status := "QrTokenCreated"
	gw, _ := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":{"Status":"` + status + `"}}`))
	})
	if err := gw.Void(context.Background(), "991"); err != nil {
		t.Fatalf("Void() on an unpaid link error = %v, want nil", err)
	}

	status = "Processed"
	err := gw.Void(context.Background(), "991")
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("error = %v, want a refusal — voiding a paid Kaspi payment must never silently refund", err)
	}
}

func TestErrorsCarryNoCredentials(t *testing.T) {
	gw, srv := newTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"` + testAPIKey + `"}`))
	})
	_, err := gw.Authorize(context.Background(), authorizeReq(100))
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, secret := range []string{testAPIKey, testSecret, srv.URL} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error text leaks a credential or the URL: %q", msg)
		}
	}
}

func TestConfigFromEnvParsesCompanyPairs(t *testing.T) {
	t.Setenv("KASPI_COMPANY_API_KEYS", " 7=kpk_one , 8=kpk_two ")
	t.Setenv("KASPI_WEBHOOK_SECRETS", "single_shared_secret")
	cfg := ConfigFromEnv()
	if cfg.CompanyAPIKeys["7"] != "kpk_one" || cfg.CompanyAPIKeys["8"] != "kpk_two" {
		t.Fatalf("company keys = %v", cfg.CompanyAPIKeys)
	}
	// A bare value is the single-company shorthand.
	if cfg.WebhookSecrets[""] != "single_shared_secret" {
		t.Fatalf("webhook secrets = %v", cfg.WebhookSecrets)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestMinChargeableUnitIsAWholeTenge(t *testing.T) {
	gw, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {})
	if got := gw.MinChargeableUnitMinor(); got != 100 {
		t.Fatalf("MinChargeableUnitMinor() = %d, want 100", got)
	}
}
