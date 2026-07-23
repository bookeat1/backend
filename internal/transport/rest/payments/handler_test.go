package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	paymentgw "backend-core/internal/infrastructure/payment"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/payments"
)

// This suite runs against a real Postgres (testdb.Connect skips otherwise):
// domain.PaymentRepository is a large, CAS-heavy interface, and hand-rolled
// fakes for it would risk drifting from the real semantics on exactly the
// money-safety properties this package must not get wrong. The acquirer is a
// hand-written fake (fakeAcquirerGateway below) — no network, no real
// FreedomPay/TipTopPay adapter needed for these tests; the webhook signature
// tests in webhook_test.go use the real adapters instead.

// fakeAcquirerGateway is a minimal domain.PaymentGateway: Authorize succeeds
// with a synthetic provider id, Capture/Void/Refund succeed, Get is unused by
// these tests. VerifyWebhook always errors — the webhook route is exercised
// with the real adapters in webhook_test.go, not through this fake.
type fakeAcquirerGateway struct{ name domain.PaymentProvider }

func (f fakeAcquirerGateway) Authorize(_ context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	return &domain.GatewayPayment{
		ProviderPaymentID: "gw-" + req.PaymentID.String(),
		PaymentURL:        "https://pay.example/" + req.PaymentID.String(),
		Status:            domain.PaymentCreated,
	}, nil
}
func (f fakeAcquirerGateway) Capture(_ context.Context, providerID string, amount domain.Money) (*domain.GatewayPayment, error) {
	return &domain.GatewayPayment{ProviderPaymentID: providerID, Status: domain.PaymentCaptured}, nil
}
func (f fakeAcquirerGateway) Void(context.Context, string) error { return nil }
func (f fakeAcquirerGateway) Refund(_ context.Context, providerID string, amount domain.Money) (*domain.GatewayRefund, error) {
	return &domain.GatewayRefund{ProviderRefundID: "rf-" + providerID, Status: domain.RefundSucceeded}, nil
}
func (f fakeAcquirerGateway) Get(context.Context, string) (*domain.GatewayPayment, error) {
	return nil, domain.ErrNotFound
}
func (f fakeAcquirerGateway) VerifyWebhook([]byte, map[string]string) (*domain.WebhookEvent, error) {
	return nil, domain.ErrValidation
}
func (f fakeAcquirerGateway) Name() domain.PaymentProvider { return f.name }

// fakeManagerChecker is the tiny managerChecker port.
type fakeManagerChecker struct{ manages bool }

func (f fakeManagerChecker) Manages(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return f.manages, nil
}

// fakeCancelDeadline is the tiny cancelDeadlineResolver port: a booking may
// always be cancelled up to deadline, regardless of its own StartsAt — good
// enough for handler-level tests, which never assert on the exact settlement
// split (that belongs to usecase/payments' own tests).
type fakeCancelDeadline struct{ deadline time.Time }

func (f fakeCancelDeadline) CancelDeadlineFor(context.Context, domain.Booking) (time.Time, error) {
	return f.deadline, nil
}

// testEnv wires the real Postgres repositories and real usecases behind the
// transport handler, mirroring bootstrap.NewDeps/NewApp closely enough to
// exercise the same wiring this package will actually run with.
type testEnv struct {
	pool     *pgxpool.Pool
	handler  *Handler
	manager  *fakeManagerChecker
	baseURL  string
	provider domain.PaymentProvider
}

func newTestEnvWithConfig(t *testing.T, mutate func(*uc.Config)) *testEnv {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "payment_outbox", "payment_ledger_entries", "payment_events",
		"payment_refunds", "payments", "bookings", "restaurants", "restaurant_categories")
	// Both providers are seeded disabled by migration 0007; enable freedompay
	// as the default for these tests.
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_providers SET is_enabled=true, is_default=true WHERE provider='freedompay'`); err != nil {
		t.Fatalf("enable freedompay: %v", err)
	}
	return newTestEnvWithConfigSamePool(t, pool, mutate)
}

// newTestEnvWithConfigSamePool builds a second Handler (a "second deploy")
// against an ALREADY-SEEDED pool, without truncating it — used to prove that
// flipping PAYMENTS_ENABLED between deploys does not strand a payment a
// PREVIOUS deploy already put a hold on.
func newTestEnvWithConfigSamePool(t *testing.T, pool *pgxpool.Pool, mutate func(*uc.Config)) *testEnv {
	t.Helper()
	paymentsRepo := paymentrepo.New(pool)
	refundsRepo := paymentrepo.NewRefunds(pool)
	eventsRepo := paymentrepo.NewEvents(pool)
	ledgerRepo := paymentrepo.NewLedger(pool)
	outboxRepo := paymentrepo.NewOutbox(pool)
	providersRepo := paymentrepo.NewProviders(pool)

	bookingRepo := bookingrepo.New(pool)
	bookingItems := bookingrepo.NewItems(pool)
	restRepo := restrepo.New(pool)

	registry, err := paymentgw.NewRegistry(providersRepo, domain.ProviderFreedomPay,
		fakeAcquirerGateway{name: domain.ProviderFreedomPay})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	txm := sqltx.NewManager(pool)
	managers := &fakeManagerChecker{}
	deadline := fakeCancelDeadline{deadline: time.Now().Add(48 * time.Hour)}

	cfg := uc.Config{
		Enabled: true, DefaultProvider: domain.ProviderFreedomPay, ServiceFeeBps: 350,
		RefundAcquiringBps: 100, DepositDefaultMinor: 10000, DepositRequired: true,
		HoldTTL: time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	create := uc.NewCreateUseCase(paymentsRepo, outboxRepo, bookingRepo, bookingItems, restRepo, registry, managers, txm, cfg)
	capture := uc.NewCaptureUseCase(paymentsRepo, ledgerRepo, outboxRepo, registry, managers, txm)
	void := uc.NewVoidUseCase(paymentsRepo, outboxRepo, registry, managers, txm)
	refund := uc.NewRefundUseCase(paymentsRepo, refundsRepo, ledgerRepo, outboxRepo, registry, managers, bookingRepo, deadline, txm, cfg)
	webhook := uc.NewWebhookUseCase(paymentsRepo, eventsRepo, ledgerRepo, outboxRepo, registry, txm)
	status := uc.NewStatusUseCase(paymentsRepo, managers)

	h := NewHandler(create, capture, void, refund, webhook, status, registry, "https://api.bookeat.test")

	return &testEnv{pool: pool, handler: h, manager: managers, baseURL: "https://api.bookeat.test", provider: domain.ProviderFreedomPay}
}

func newTestEnv(t *testing.T) *testEnv { return newTestEnvWithConfig(t, nil) }

func (e *testEnv) router(role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")

	guest := api.Group("")
	guest.Use(middleware.OptionalAuth(fakeIssuer{}, fakeUsers{role: role}))
	e.handler.RegisterGuestRoutes(guest)

	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	e.handler.RegisterStaffRoutes(authed)

	e.handler.RegisterWebhooks(r.Group("/"))
	return r
}

func (e *testEnv) seedRestaurant(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	restRepo := restrepo.New(e.pool)
	m := &domain.Restaurant{
		ID: id, Name: "Payment Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := restRepo.Create(context.Background(), m); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

// seedUser inserts a users row so a booking (or a payment) may legally
// reference it — bookings.user_id carries a real FK.
func (e *testEnv) seedUser(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
		id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func (e *testEnv) seedBooking(t *testing.T, restaurantID uuid.UUID, userID *uuid.UUID) *domain.Booking {
	t.Helper()
	if userID != nil {
		e.seedUser(t, *userID)
	}
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: restaurantID, UserID: userID, Name: "Гость",
		Phone: "+7 (777) 123-45-67", Email: "guest@example.com", PhoneNormalized: "+77771234567",
		Guests: 2, StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(26 * time.Hour),
		Status: domain.BookingConfirmed, Source: domain.SourceApp,
	}
	repo := bookingrepo.New(e.pool)
	if err := repo.Create(context.Background(), b); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return b
}

func doJSON(r *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func authHeader(userID uuid.UUID) map[string]string {
	return map[string]string{"Authorization": "Bearer " + userID.String()}
}

func decodeData(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %s)", err, w.Body.String())
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decode data: %v (body: %s)", err, w.Body.String())
	}
}

// TestCreatePayment_GuestAnonymousCheckout covers the core guest path: no
// bearer token at all, still allowed to pay for their own (accountless)
// booking.
func TestCreatePayment_GuestAnonymousCheckout(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	r := env.router(domain.RoleUser)

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"},
		map[string]string{idempotencyHeader: "guest-key-1"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	var resp paymentResponse
	decodeData(t, w, &resp)
	if resp.Status != string(domain.PaymentCreated) {
		t.Errorf("status = %q, want created", resp.Status)
	}
	if resp.PaymentURL == nil || *resp.PaymentURL == "" {
		t.Error("expected a payment_url to redirect the guest to")
	}
	if resp.AmountMinor != 10350 { // 10000 deposit + 3.5% fee, rounded up
		t.Errorf("amount_minor = %d, want 10350", resp.AmountMinor)
	}
}

// TestCreatePayment_MissingReturnURLRejected covers server-side validation:
// return_url is required.
func TestCreatePayment_MissingReturnURLRejected(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	r := env.router(domain.RoleUser)

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{}, map[string]string{idempotencyHeader: "k"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

// TestCreatePayment_NoPublicBaseURLConfigured proves the transport refuses to
// silently authorize a hold it cannot ever receive a webhook for.
func TestCreatePayment_NoPublicBaseURLConfigured(t *testing.T) {
	env := newTestEnv(t)
	env.handler.publicBaseURL = ""
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	r := env.router(domain.RoleUser)

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, map[string]string{idempotencyHeader: "k"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 when the webhook callback cannot be built", w.Code)
	}
}

// TestStatusByID_GuestCannotReadSomeoneElsesPayment is one of the required
// tests: a guest must never read a payment that is not theirs.
func TestStatusByID_GuestCannotReadSomeoneElsesPayment(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	owner := uuid.New()
	b := env.seedBooking(t, rid, &owner)
	r := env.router(domain.RoleUser)

	// The owner creates the payment.
	ownerHeaders := authHeader(owner)
	ownerHeaders[idempotencyHeader] = "owner-key"
	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, ownerHeaders)
	if w.Code != http.StatusCreated {
		t.Fatalf("owner create: status = %d, body %s", w.Code, w.Body.String())
	}
	var created paymentResponse
	decodeData(t, w, &created)

	// A different logged-in guest must not be able to read it.
	stranger := uuid.New()
	w2 := doJSON(r, http.MethodGet, "/api/v1/payments/"+created.ID, nil, authHeader(stranger))
	if w2.Code != http.StatusNotFound {
		t.Errorf("stranger read status = %d, want 404 (no enumeration oracle)", w2.Code)
	}

	// A fully anonymous caller (no bearer token at all) must not either.
	w3 := doJSON(r, http.MethodGet, "/api/v1/payments/"+created.ID, nil, nil)
	if w3.Code != http.StatusNotFound {
		t.Errorf("anonymous read status = %d, want 404", w3.Code)
	}

	// The owner themselves can.
	w4 := doJSON(r, http.MethodGet, "/api/v1/payments/"+created.ID, nil, authHeader(owner))
	if w4.Code != http.StatusOK {
		t.Errorf("owner read status = %d, want 200", w4.Code)
	}
}

// TestCaptureHold_StaffOfAnotherRestaurantForbidden is one of the required
// tests: staff of a different venue must not be able to capture a hold.
func TestCaptureHold_StaffOfAnotherRestaurantForbidden(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	guestRouter := env.router(domain.RoleUser)

	w := doJSON(guestRouter, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, map[string]string{idempotencyHeader: "cap-key"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", w.Code, w.Body.String())
	}

	// Authorize the hold via the fake webhook callback so it becomes capturable.
	env.authorizeViaWebhookEvent(t, w)

	env.manager.manages = false // staff manages some OTHER restaurant
	staffRouter := env.router(domain.RoleRestaurant)
	staffID := uuid.New()
	w2 := doJSON(staffRouter, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/capture", nil, authHeader(staffID))
	if w2.Code != http.StatusForbidden {
		t.Errorf("capture by another venue's staff status = %d, want 403 (body %s)", w2.Code, w2.Body.String())
	}

	env.manager.manages = true
	w3 := doJSON(staffRouter, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/capture", nil, authHeader(staffID))
	if w3.Code != http.StatusOK {
		t.Errorf("capture by the right venue's staff status = %d, want 200 (body %s)", w3.Code, w3.Body.String())
	}
}

// TestVoidHold_RequiresStaff proves an anonymous guest cannot void a hold.
func TestVoidHold_RequiresStaff(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	r := env.router(domain.RoleUser)

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/void", voidRequest{}, nil)
	// No bearer token at all on a route mounted behind middleware.Auth: the
	// middleware itself rejects with 401 before the usecase is ever reached.
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// authorizeViaWebhookEvent moves the just-created payment from `created` to
// `authorized` directly through SQL — a handler test has no real acquirer to
// deliver a webhook from, and CaptureOnSeating requires `authorized`.
func (e *testEnv) authorizeViaWebhookEvent(t *testing.T, createResp *httptest.ResponseRecorder) {
	t.Helper()
	var created paymentResponse
	decodeData(t, createResp, &created)
	_, err := e.pool.Exec(context.Background(),
		`UPDATE payments SET status='authorized', authorized_at=now(), status_changed_at=now() WHERE id=$1`, created.ID)
	if err != nil {
		t.Fatalf("authorize payment: %v", err)
	}
}

// captureViaSQL moves a payment straight to `captured` (skipping the real
// acquirer flow, same reasoning as authorizeViaWebhookEvent) so Settle tests
// can exercise a payment that is actually eligible for settlement.
func (e *testEnv) captureViaSQL(t *testing.T, paymentID uuid.UUID) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE payments SET status='captured', authorized_at=now(), captured_at=now(), status_changed_at=now() WHERE id=$1`,
		paymentID); err != nil {
		t.Fatalf("capture payment via SQL: %v", err)
	}
}

// TestSettlePayment_GuestCancelBeforeDeadlineRefundsOnce is one of the
// required tests: a guest cancellation before the deadline refunds the guest
// (minus the acquiring cost), and a retried request with the SAME idempotency
// key never refunds a second time.
func TestSettlePayment_GuestCancelBeforeDeadlineRefundsOnce(t *testing.T) {
	env := newTestEnv(t)
	rid := env.seedRestaurant(t)
	owner := uuid.New()
	b := env.seedBooking(t, rid, &owner)
	r := env.router(domain.RoleUser)
	ownerHeaders := authHeader(owner)
	ownerHeaders[idempotencyHeader] = "create-key"

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, ownerHeaders)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", w.Code, w.Body.String())
	}
	var created paymentResponse
	decodeData(t, w, &created)
	pid, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatalf("parse payment id: %v", err)
	}
	env.captureViaSQL(t, pid)
	// A guest-cancel settlement needs a recorded cancellation on the booking
	// (usecase/payments.resolveTiming) — set directly, the booking's own
	// cancel flow (usecase/bookings) is out of scope for this transport test.
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE bookings SET status='cancelled', cancelled_at=now() WHERE id=$1`, b.ID); err != nil {
		t.Fatalf("mark booking cancelled: %v", err)
	}

	settleHeaders := authHeader(owner)
	settleHeaders[idempotencyHeader] = "settle-key"
	settleBody := settleRequest{Trigger: string(domain.RefundTriggerGuestCancel)}

	w1 := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/settle", settleBody, settleHeaders)
	if w1.Code != http.StatusOK {
		t.Fatalf("first settle: status = %d, body %s", w1.Code, w1.Body.String())
	}
	var settled1 paymentResponse
	decodeData(t, w1, &settled1)
	if settled1.Status != string(domain.PaymentRefunded) {
		t.Errorf("status = %q, want refunded", settled1.Status)
	}

	// Retry with the SAME idempotency key, routed by booking id (the only
	// route this transport exposes for Settle). RefundUseCase.Settle resolves
	// the payment via domain.PaymentRepository.GetSettleableByBookingID
	// (report/bug fix: it used to call GetLiveByBookingID, which stops
	// finding a payment the moment it becomes `refunded` — see that method's
	// doc comment), so this retry must resume idempotently: same 200, the
	// same already-refunded payment, and — checked below — no second row in
	// payment_refunds.
	w2 := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/settle", settleBody, settleHeaders)
	if w2.Code != http.StatusOK {
		t.Fatalf("retried settle (same idempotency key, after full settlement) status = %d, want 200 (idempotent resume): %s", w2.Code, w2.Body.String())
	}
	var settled2 paymentResponse
	decodeData(t, w2, &settled2)
	if settled2.ID != settled1.ID || settled2.Status != string(domain.PaymentRefunded) {
		t.Errorf("retried settle returned %+v, want the same refunded payment as the first call", settled2)
	}

	var refundCount int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_refunds WHERE payment_id=$1`, pid).Scan(&refundCount); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if refundCount != 1 {
		t.Errorf("payment_refunds rows for this payment = %d, want exactly 1 (no double refund, regardless of the 404 above)", refundCount)
	}
}

// TestCreatePayment_PaymentsGloballyDisabled documents the chosen behaviour
// for PAYMENTS_ENABLED=false (see the package doc): Create reports a precise,
// honest validation error instead of a blanket "feature off".
func TestCreatePayment_PaymentsGloballyDisabled(t *testing.T) {
	env := newTestEnvWithConfig(t, func(cfg *uc.Config) { cfg.Enabled = false })
	rid := env.seedRestaurant(t)
	b := env.seedBooking(t, rid, nil)
	r := env.router(domain.RoleUser)

	w := doJSON(r, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, map[string]string{idempotencyHeader: "k"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (payments not enabled)", w.Code)
	}
}

// TestCaptureHold_StillWorksWhenPaymentsGloballyDisabled proves the decision
// documented in the package doc: a payment that was already held while
// payments were enabled can still be captured after PAYMENTS_ENABLED flips to
// false on a later deploy — the switch must not strand money already held.
// Modelled as two environments sharing the same database: one that created
// the hold (Enabled=true), one that captures it (Enabled=false).
func TestCaptureHold_StillWorksWhenPaymentsGloballyDisabled(t *testing.T) {
	enabledEnv := newTestEnv(t) // Enabled=true
	rid := enabledEnv.seedRestaurant(t)
	b := enabledEnv.seedBooking(t, rid, nil)
	guestRouter := enabledEnv.router(domain.RoleUser)

	w := doJSON(guestRouter, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment",
		createPaymentRequest{ReturnURL: "bookeat://return"}, map[string]string{idempotencyHeader: "k"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", w.Code, w.Body.String())
	}
	enabledEnv.authorizeViaWebhookEvent(t, w)

	// A second environment against the SAME pool (no truncation), representing
	// a later deploy with PAYMENTS_ENABLED=false.
	disabledEnv := newTestEnvWithConfigSamePool(t, enabledEnv.pool, func(cfg *uc.Config) { cfg.Enabled = false })
	disabledEnv.manager.manages = true
	staffRouter := disabledEnv.router(domain.RoleRestaurant)
	w2 := doJSON(staffRouter, http.MethodPost, "/api/v1/bookings/"+b.ID.String()+"/payment/capture", nil, authHeader(uuid.New()))
	if w2.Code != http.StatusOK {
		t.Errorf("capture status = %d, want 200 even with PAYMENTS_ENABLED=false on this deploy (body %s)", w2.Code, w2.Body.String())
	}
}
