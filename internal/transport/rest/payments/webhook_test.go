package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	paymentgw "backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
	"backend-core/internal/infrastructure/payment/tiptoppay"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	uc "backend-core/internal/usecase/payments"
)

// This suite exercises the two webhook routes with the REAL acquirer adapters
// (not fakes) specifically so the signature verification and the wire-format
// acknowledgement are genuine, not asserted against a stand-in.

const (
	testFreedomPaySecret = "test-freedompay-secret-do-not-log"
	testFreedomPayScript = freedompay.DefaultResultScriptName // "freedompay" — must match the route
	testTipTopSecret     = "test-tiptoppay-secret-do-not-log"
)

// freedomPaySign replicates freedompay's (unexported) signature algorithm —
// MD5("script;<fields sorted by name, incl. pg_salt>;secret"), hex — so this
// test package can build a genuinely, independently signed callback without
// reaching into freedompay's internals.
func freedomPaySign(script string, params url.Values, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "pg_sig" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{script}
	for _, k := range keys {
		parts = append(parts, params[k]...)
	}
	parts = append(parts, secret)
	sum := md5.Sum([]byte(strings.Join(parts, ";")))
	return hex.EncodeToString(sum[:])
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// signedFreedomPayCallback builds a genuinely signed result_url body.
func signedFreedomPayCallback(t *testing.T, fields map[string]string) []byte {
	t.Helper()
	params := url.Values{}
	for k, v := range fields {
		params.Set(k, v)
	}
	params.Set("pg_salt", randomHex(t, 16))
	params.Set("pg_sig", freedomPaySign(testFreedomPayScript, params, testFreedomPaySecret))
	return []byte(params.Encode())
}

// freedomPayXML is the shape of the signed <response> body (see
// freedompay.Gateway.ack) — enough to parse pg_status and verify pg_sig.
type freedomPayXML struct {
	XMLName xml.Name `xml:"response"`
	Status  string   `xml:"pg_status"`
	Salt    string   `xml:"pg_salt"`
	Sig     string   `xml:"pg_sig"`
}

func parseFreedomPayAck(t *testing.T, body []byte) freedomPayXML {
	t.Helper()
	var out freedomPayXML
	if err := xml.Unmarshal(body, &out); err != nil {
		t.Fatalf("ack is not valid XML: %v (body: %s)", err, body)
	}
	return out
}

// webhookTestEnv wires the real FreedomPay/TipTopPay adapters, a real
// Postgres-backed WebhookUseCase, and the transport Handler's webhook routes.
type webhookTestEnv struct {
	pool    *pgxpool.Pool
	handler *Handler
	router  *gin.Engine
}

func newWebhookTestEnv(t *testing.T) *webhookTestEnv {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "payment_outbox", "payment_ledger_entries", "payment_events",
		"payment_refunds", "payments", "bookings", "restaurants", "restaurant_categories")
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_providers SET is_enabled=true, is_default=true WHERE provider='freedompay'`); err != nil {
		t.Fatalf("enable freedompay: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_providers SET is_enabled=true WHERE provider='tiptoppay'`); err != nil {
		t.Fatalf("enable tiptoppay: %v", err)
	}

	paymentsRepo := paymentrepo.New(pool)
	eventsRepo := paymentrepo.NewEvents(pool)
	ledgerRepo := paymentrepo.NewLedger(pool)
	outboxRepo := paymentrepo.NewOutbox(pool)
	providersRepo := paymentrepo.NewProviders(pool)

	fpGW, err := freedompay.New(freedompay.Config{
		BaseURL: freedompay.DefaultBaseURL, MerchantID: "test-merchant",
		SecretKey: testFreedomPaySecret, ResultScriptName: testFreedomPayScript,
	}, nil, nil)
	if err != nil {
		t.Fatalf("build freedompay gateway: %v", err)
	}
	ttpGW, err := tiptoppay.New(tiptoppay.Config{
		BaseURL: tiptoppay.DefaultBaseURL, PublicID: "test-public-id", APISecret: testTipTopSecret,
	}, nil, nil)
	if err != nil {
		t.Fatalf("build tiptoppay gateway: %v", err)
	}

	registry, err := paymentgw.NewRegistry(providersRepo, domain.ProviderFreedomPay, fpGW, ttpGW)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	webhook := uc.NewWebhookUseCase(paymentsRepo, eventsRepo, ledgerRepo, outboxRepo, registry, sqltx.NewManager(pool))
	h := NewHandler(nil, nil, nil, nil, webhook, nil, registry, "https://api.bookeat.test")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterWebhooks(r.Group("/"))

	return &webhookTestEnv{pool: pool, handler: h, router: r}
}

// seedAuthorizedPayment inserts a payment row already in `authorized`, with a
// known provider_payment_id, so a webhook can apply a real transition against
// it (a webhook must never conjure a payment out of thin air — spec §7 — so
// there has to be a row to resolve to).
func (e *webhookTestEnv) seedAuthorizedPayment(t *testing.T, provider domain.PaymentProvider, providerPaymentID string) uuid.UUID {
	t.Helper()
	restaurantID := uuid.New()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, restaurantID); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	bookingID := uuid.New()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, email, phone_normalized, guests, starts_at, ends_at, status, source)
		 VALUES ($1,$2,'Гость','+77771234567','g@example.com','+77771234567',2, now()+interval '1 day', now()+interval '1 day 2 hours', 'confirmed', 'app')`,
		bookingID, restaurantID); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	paymentID := uuid.New()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO payments (id, booking_id, restaurant_id, provider, provider_payment_id, purpose, status,
		 amount_minor, base_amount_minor, fee_minor, currency, idempotency_key, authorized_at, status_changed_at)
		 VALUES ($1,$2,$3,$4,$5,'deposit','authorized',10350,10000,350,'KZT',$6,now(),now())`,
		paymentID, bookingID, restaurantID, string(provider), providerPaymentID, "idem-"+paymentID.String()); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return paymentID
}

func (e *webhookTestEnv) paymentStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM payments WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read payment status: %v", err)
	}
	return status
}

func (e *webhookTestEnv) eventCount(t *testing.T, provider domain.PaymentProvider) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_events WHERE provider=$1`, string(provider)).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func doRaw(r *gin.Engine, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestFreedomPayWebhook_ValidSignatureCaptures is a required test: a
// correctly signed callback applies (here: hold → captured) and the
// acknowledgement is a valid, signed XML envelope with pg_status=ok.
func TestFreedomPayWebhook_ValidSignatureCaptures(t *testing.T) {
	env := newWebhookTestEnv(t)
	providerPaymentID := "1800000001"
	pid := env.seedAuthorizedPayment(t, domain.ProviderFreedomPay, providerPaymentID)

	body := signedFreedomPayCallback(t, map[string]string{
		"pg_payment_id": providerPaymentID, "pg_order_id": pid.String(),
		"pg_amount": "103.50", "pg_currency": "KZT", "pg_result": "1", "pg_captured": "1",
		"pg_payment_date": "2026-07-23 12:00:00",
	})

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/freedompay", body,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	ack := parseFreedomPayAck(t, w.Body.Bytes())
	if ack.Status != "ok" {
		t.Errorf("pg_status = %q, want ok (full body: %s)", ack.Status, w.Body.String())
	}
	if ack.Sig == "" || ack.Salt == "" {
		t.Errorf("ack is not signed: %+v", ack)
	}
	// The ack must itself be validly signed with the same secret/script.
	ackParams := url.Values{"pg_status": {ack.Status}, "pg_salt": {ack.Salt}}
	if got := freedomPaySign(testFreedomPayScript, ackParams, testFreedomPaySecret); got != ack.Sig {
		t.Errorf("ack signature does not verify: got sig %q, computed %q", ack.Sig, got)
	}

	if got := env.paymentStatus(t, pid); got != string(domain.PaymentCaptured) {
		t.Errorf("payment status = %q, want captured", got)
	}
}

// TestFreedomPayWebhook_InvalidSignatureRejected is a required test: a bad
// signature must not be applied, and the ack must not reveal why.
func TestFreedomPayWebhook_InvalidSignatureRejected(t *testing.T) {
	env := newWebhookTestEnv(t)
	providerPaymentID := "1800000002"
	pid := env.seedAuthorizedPayment(t, domain.ProviderFreedomPay, providerPaymentID)

	params := url.Values{}
	params.Set("pg_payment_id", providerPaymentID)
	params.Set("pg_result", "1")
	params.Set("pg_captured", "1")
	params.Set("pg_salt", randomHex(t, 16))
	params.Set("pg_sig", "0000000000000000000000000000000000000000000000000000000000000000") // wrong

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/freedompay", []byte(params.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (FreedomPay always gets 200 + a signed body, per spec)", w.Code)
	}
	ack := parseFreedomPayAck(t, w.Body.Bytes())
	if ack.Status != "error" {
		t.Errorf("pg_status = %q, want error for a bad signature", ack.Status)
	}
	if strings.Contains(w.Body.String(), "signature") || strings.Contains(w.Body.String(), "sig") && strings.Contains(w.Body.String(), "verif") {
		t.Errorf("ack must not reveal the failure reason: %s", w.Body.String())
	}
	if got := env.paymentStatus(t, pid); got != string(domain.PaymentAuthorized) {
		t.Errorf("payment status = %q, want unchanged (authorized) after a bad-signature callback", got)
	}
	if n := env.eventCount(t, domain.ProviderFreedomPay); n != 1 {
		t.Errorf("payment_events rows = %d, want 1 (the bad-signature attempt is still recorded as evidence)", n)
	}
}

// TestFreedomPayWebhook_RedeliveryDoesNotCaptureTwice is a required test:
// FreedomPay retries a callback every 30 minutes for 2 hours (sandbox-
// confirmed) — the SAME logical event, redelivered, must be a no-op the
// second time.
func TestFreedomPayWebhook_RedeliveryDoesNotCaptureTwice(t *testing.T) {
	env := newWebhookTestEnv(t)
	providerPaymentID := "1800000003"
	pid := env.seedAuthorizedPayment(t, domain.ProviderFreedomPay, providerPaymentID)

	fields := map[string]string{
		"pg_payment_id": providerPaymentID, "pg_order_id": pid.String(),
		"pg_amount": "103.50", "pg_currency": "KZT", "pg_result": "1", "pg_captured": "1",
	}
	// FreedomPay's own retry regenerates pg_salt/pg_sig each time but the
	// EVENT identity (see freedompay.VerifyWebhook's ProviderEventID doc
	// comment) is derived from the stable facts, not the salt — so two
	// independently-signed deliveries of "the same event" must still
	// deduplicate.
	first := signedFreedomPayCallback(t, fields)
	second := signedFreedomPayCallback(t, fields)
	if bytes.Equal(first, second) {
		t.Fatal("test setup bug: the two deliveries must differ (different pg_salt/pg_sig), like a real retry")
	}

	w1 := doRaw(env.router, http.MethodPost, "/webhooks/payments/freedompay", first, nil)
	if w1.Code != http.StatusOK || parseFreedomPayAck(t, w1.Body.Bytes()).Status != "ok" {
		t.Fatalf("first delivery: status=%d body=%s", w1.Code, w1.Body.String())
	}
	w2 := doRaw(env.router, http.MethodPost, "/webhooks/payments/freedompay", second, nil)
	if w2.Code != http.StatusOK || parseFreedomPayAck(t, w2.Body.Bytes()).Status != "ok" {
		t.Fatalf("redelivery: status=%d body=%s", w2.Code, w2.Body.String())
	}

	if n := env.eventCount(t, domain.ProviderFreedomPay); n != 1 {
		t.Errorf("payment_events rows = %d, want exactly 1 for the redelivered event", n)
	}
	if got := env.paymentStatus(t, pid); got != string(domain.PaymentCaptured) {
		t.Errorf("payment status = %q, want captured (unchanged by the redelivery)", got)
	}
}

// TestFreedomPayWebhook_BodyTooLargeRejected is a required test.
func TestFreedomPayWebhook_BodyTooLargeRejected(t *testing.T) {
	env := newWebhookTestEnv(t)
	oversized := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1)

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/freedompay", oversized, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// TestTipTopPayWebhook_ValidSignatureCaptures is a required test.
func TestTipTopPayWebhook_ValidSignatureCaptures(t *testing.T) {
	env := newWebhookTestEnv(t)
	txID := "770000001"
	pid := env.seedAuthorizedPayment(t, domain.ProviderTipTopPay, txID)

	body := url.Values{
		"TransactionId": {txID}, "InvoiceId": {pid.String()}, "Amount": {"103.50"},
		"Currency": {"KZT"}, "Status": {"Completed"}, "DateTime": {"2026-07-23 12:00:00"},
	}.Encode()

	mac := hmac.New(sha256.New, []byte(testTipTopSecret))
	mac.Write([]byte(body))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/tiptoppay/confirm", []byte(body),
		map[string]string{"Content-HMAC": sig, "Content-Type": "application/x-www-form-urlencoded"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var ack map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatalf("ack is not valid JSON: %v (body %s)", err, w.Body.String())
	}
	if ack["code"] != 0 {
		t.Errorf("ack = %v, want {\"code\":0}", ack)
	}
	if got := env.paymentStatus(t, pid); got != string(domain.PaymentCaptured) {
		t.Errorf("payment status = %q, want captured", got)
	}
}

// TestTipTopPayWebhook_InvalidSignatureRejected is a required test: TipTopPay
// always gets {"code":0} (see the handler's doc comment on why), but the
// event must not be applied.
func TestTipTopPayWebhook_InvalidSignatureRejected(t *testing.T) {
	env := newWebhookTestEnv(t)
	txID := "770000002"
	pid := env.seedAuthorizedPayment(t, domain.ProviderTipTopPay, txID)

	body := url.Values{"TransactionId": {txID}, "InvoiceId": {pid.String()}, "Status": {"Completed"}}.Encode()

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/tiptoppay/confirm", []byte(body),
		map[string]string{"Content-HMAC": "not-a-real-signature"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (always {\"code\":0})", w.Code)
	}
	var ack map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &ack)
	if ack["code"] != 0 {
		t.Errorf("ack = %v, want {\"code\":0} even for a bad signature", ack)
	}
	if got := env.paymentStatus(t, pid); got != string(domain.PaymentAuthorized) {
		t.Errorf("payment status = %q, want unchanged (authorized) after a bad-signature notification", got)
	}
}

// TestTipTopPayWebhook_BodyTooLargeRejected is a required test.
func TestTipTopPayWebhook_BodyTooLargeRejected(t *testing.T) {
	env := newWebhookTestEnv(t)
	oversized := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1)

	w := doRaw(env.router, http.MethodPost, "/webhooks/payments/tiptoppay/confirm", oversized, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}
