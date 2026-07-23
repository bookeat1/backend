package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func testPayment(bookingID uuid.UUID, status domain.PaymentStatus, providerPaymentID string) *domain.Payment {
	pid := providerPaymentID
	now := time.Now()
	return &domain.Payment{
		ID: uuid.New(), BookingID: bookingID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: &pid,
		Purpose: domain.PurposeDeposit, Status: status,
		AmountMinor: 1_035_000, BaseAmountMinor: 1_000_000, FeeMinor: 35_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: bookingID.String() + ":k",
		CreatedAt: now, UpdatedAt: now,
	}
}

func newWebhookHarness(payments ...*domain.Payment) (*webhookUseCase, *fakePaymentRepo, *fakeEventRepo, *fakeLedgerRepo, *fakePaymentOutbox, *fakeGateway) {
	repo := newFakePaymentRepo(payments...)
	events := newFakeEventRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	u := NewWebhookUseCase(repo, events, ledger, outbox, resolver, tx).(*webhookUseCase)
	return u, repo, events, ledger, outbox, gw
}

func verifyOK(ev *domain.WebhookEvent) func([]byte, map[string]string) (*domain.WebhookEvent, error) {
	return func([]byte, map[string]string) (*domain.WebhookEvent, error) { return ev, nil }
}

func TestHandleWebhook_AuthorizedHappyPath(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, _, _, outbox, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1", ProviderPaymentID: "gw-1",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})

	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want authorized", stored.Status)
	}
	if stored.AuthorizedAt == nil {
		t.Fatalf("AuthorizedAt not set")
	}
	found := false
	for _, ty := range outbox.types() {
		if ty == domain.EventPaymentAuthorized {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want payment.authorized", outbox.types())
	}
}

func TestHandleWebhook_DuplicateDeliveryIsNoOp(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, _, _, outbox, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1", ProviderPaymentID: "gw-1",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	ctx := context.Background()

	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("first delivery error = %v", err)
	}
	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("body-retried-bytes-may-differ"), nil); err != nil {
		t.Fatalf("second delivery error = %v", err)
	}
	if len(outbox.types()) != 1 {
		t.Fatalf("outbox got %d events across 2 deliveries of the same provider_event_id, want 1", len(outbox.types()))
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want authorized", stored.Status)
	}
}

func TestHandleWebhook_BadSignatureIsStoredAndRejected(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, events, _, outbox, gw := newWebhookHarness(p)
	sigErr := errors.New("freedompay: webhook signature verification failed")
	gw.verifyFn = func([]byte, map[string]string) (*domain.WebhookEvent, error) { return nil, sigErr }

	err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("forged"), nil)
	if err == nil {
		t.Fatalf("expected an error for a bad signature")
	}
	if len(events.byID) != 1 {
		t.Fatalf("payment_events got %d rows, want 1 (evidence must still be stored)", len(events.byID))
	}
	for _, e := range events.byID {
		if e.SignatureValid {
			t.Fatalf("stored event has signature_valid=true, want false")
		}
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from an unverified callback, want 0", len(outbox.types()))
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCreated {
		t.Fatalf("status changed to %s from an unverified callback, want unchanged", stored.Status)
	}
}

func TestHandleWebhook_UnknownPaymentNeverCreatesOne(t *testing.T) {
	u, repo, events, _, _, gw := newWebhookHarness() // no payments known
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-ghost", ProviderPaymentID: "gw-ghost",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})

	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v, want nil (acknowledged, not created)", err)
	}
	if len(repo.byID) != 0 {
		t.Fatalf("payments table has %d rows, want 0 — a webhook must never create a payment", len(repo.byID))
	}
	if len(events.byID) != 1 {
		t.Fatalf("payment_events got %d rows, want 1", len(events.byID))
	}
}

// TestHandleWebhook_MerchantIDResolvesOnlyOwnProvider is non-blocking item #3
// (second review): a payment is resolved by our own primary key
// (MerchantPaymentID) with no idea, from GetByID alone, which acquirer the
// callback actually came from. A TipTopPay callback whose MerchantPaymentID
// happens to name a FreedomPay payment (coincidence, misconfiguration, or an
// attacker probing the wrong provider's endpoint) must be treated as unknown,
// never applied to the wrong acquirer's payment.
func TestHandleWebhook_MerchantIDResolvesOnlyOwnProvider(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	p.Provider = domain.ProviderFreedomPay
	u, repo, events, _, outbox, gw := newWebhookHarness(p)
	// No ProviderPaymentID match (a hosted-page callback before the acquirer
	// side id is known) — only MerchantPaymentID resolves it, and this
	// webhook claims to be from a DIFFERENT provider than the payment's own.
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderTipTopPay, ProviderEventID: "evt-cross-provider",
		MerchantPaymentID: p.ID.String(),
		Type:              domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	tiptop := newFakeGateway(domain.ProviderTipTopPay)
	tiptop.verifyFn = gw.verifyFn
	resolver := newFakeGatewayResolver(gw, tiptop)
	u.gateways = resolver

	if err := u.HandleWebhook(context.Background(), domain.ProviderTipTopPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v, want nil (acknowledged as unknown, not applied)", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, want unchanged created — a cross-provider callback must never touch this payment", stored.Status)
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events, want 0", len(outbox.types()))
	}
	if len(events.byID) != 1 {
		t.Fatalf("payment_events got %d rows, want 1 (still recorded as evidence)", len(events.byID))
	}
}

// TestHandleWebhook_CapturedAmountMismatchIsNotAppliedSilently is
// non-blocking item #4 (second review): applyCaptured must not blindly book
// the payment's OWN full total when the acquirer's callback reports a
// DIFFERENT amount — a partial capture must not be silently recorded as a
// full one.
func TestHandleWebhook_CapturedAmountMismatchIsNotAppliedSilently(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	u, repo, _, ledger, outbox, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-partial", ProviderPaymentID: "gw-1",
		Type: domain.WebhookPaymentCaptured, Status: domain.PaymentCaptured, SignatureValid: true,
		Amount: domain.Money{AmountMinor: p.AmountMinor / 2, Currency: p.Currency},
	})

	err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil)
	if err == nil {
		t.Fatalf("HandleWebhook() error = nil, want an amount-mismatch error")
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want unchanged authorized — a mismatched amount must not be applied", stored.Status)
	}
	if len(ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries from a mismatched capture, want 0", len(ledger.entries))
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from a mismatched capture, want 0", len(outbox.types()))
	}
}

func TestHandleWebhook_UnknownStatusNeverReadAsPaid(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, _, _, outbox, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-weird", ProviderPaymentID: "gw-1",
		Type: domain.WebhookUnknown, Status: domain.PaymentStatus("something_new"), SignatureValid: true,
	})

	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, an unmapped event must never move the payment", stored.Status)
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events for an unknown status, want 0", len(outbox.types()))
	}
}

// TestHandleWebhook_ConcurrentAuthorizeRaceCompensatesTheLoser is the
// financially meaningful concurrency case: two DIFFERENT payments for the
// SAME booking (e.g. two checkout attempts that both reached the acquirer)
// both try to become authorized. Only one may hold the booking's money;
// idx_payments_live_per_booking is the database-level guard, and the loser's
// hold must be released, not left dangling.
func TestHandleWebhook_ConcurrentAuthorizeRaceCompensatesTheLoser(t *testing.T) {
	bookingID := uuid.New()
	p1 := testPayment(bookingID, domain.PaymentCreated, "gw-1")
	p2 := testPayment(bookingID, domain.PaymentCreated, "gw-2")
	u, repo, _, _, outbox, gw := newWebhookHarness(p1, p2)

	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	errs := make([]error, 2)
	pairs := []struct {
		id   string
		evt  string
		ppid string
	}{{p1.ID.String(), "evt-1", "gw-1"}, {p2.ID.String(), "evt-2", "gw-2"}}

	for i, pr := range pairs {
		wg.Add(1)
		go func(i int, providerEventID, providerPaymentID string) {
			defer wg.Done()
			start.Wait()
			ev := &domain.WebhookEvent{
				Provider: domain.ProviderFreedomPay, ProviderEventID: providerEventID,
				ProviderPaymentID: providerPaymentID, Type: domain.WebhookPaymentAuthorized,
				Status: domain.PaymentAuthorized, SignatureValid: true,
			}
			gwLocal := newFakeGateway(domain.ProviderFreedomPay)
			gwLocal.voidErr = gw.voidErr
			// Each delivery must resolve VerifyWebhook to ITS OWN event; a
			// single shared verifyFn closes over the right ev per goroutine.
			resolver := &fakeGatewayResolver{byProvider: map[domain.PaymentProvider]domain.PaymentGateway{domain.ProviderFreedomPay: &fakeGatewayView{base: gw, verify: verifyOK(ev)}}}
			uu := NewWebhookUseCase(repo, u.events, u.ledger, u.outbox, resolver, u.tx).(*webhookUseCase)
			errs[i] = uu.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte(providerEventID), nil)
		}(i, pr.evt, pr.ppid)
	}
	start.Done()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d error = %v", i, err)
		}
	}

	got1, _ := repo.GetByID(context.Background(), p1.ID)
	got2, _ := repo.GetByID(context.Background(), p2.ID)
	statuses := map[domain.PaymentStatus]int{got1.Status: 1, got2.Status: 1}
	if statuses[domain.PaymentAuthorized] != 1 {
		t.Fatalf("want exactly one payment authorized, got p1=%s p2=%s", got1.Status, got2.Status)
	}
	if statuses[domain.PaymentFailed] != 1 {
		t.Fatalf("want exactly one payment failed (compensated), got p1=%s p2=%s", got1.Status, got2.Status)
	}
	if gw.callCount("void") != 1 {
		t.Fatalf("void called %d times, want exactly 1 (only the loser's hold released)", gw.callCount("void"))
	}
	failedCount, authorizedCount := 0, 0
	for _, ty := range outbox.types() {
		switch ty {
		case domain.EventPaymentFailed:
			failedCount++
		case domain.EventPaymentAuthorized:
			authorizedCount++
		}
	}
	if failedCount != 1 || authorizedCount != 1 {
		t.Fatalf("outbox events = %v, want exactly one authorized and one failed", outbox.types())
	}
}

// fakeGatewayView lets two concurrent HandleWebhook calls share the same
// underlying fakeGateway (so Void call counts are observed on one place)
// while each carries its own VerifyWebhook behaviour.
type fakeGatewayView struct {
	base   *fakeGateway
	verify func([]byte, map[string]string) (*domain.WebhookEvent, error)
}

func (v *fakeGatewayView) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	return v.base.Authorize(ctx, req)
}
func (v *fakeGatewayView) Capture(ctx context.Context, id string, amount domain.Money) (*domain.GatewayPayment, error) {
	return v.base.Capture(ctx, id, amount)
}
func (v *fakeGatewayView) Void(ctx context.Context, id string) error { return v.base.Void(ctx, id) }
func (v *fakeGatewayView) Refund(ctx context.Context, id string, amount domain.Money) (*domain.GatewayRefund, error) {
	return v.base.Refund(ctx, id, amount)
}
func (v *fakeGatewayView) Get(ctx context.Context, id string) (*domain.GatewayPayment, error) {
	return v.base.Get(ctx, id)
}
func (v *fakeGatewayView) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	return v.verify(raw, headers)
}
func (v *fakeGatewayView) Name() domain.PaymentProvider { return v.base.Name() }

// TestHandleWebhook_ResumesAfterCrashBetweenInsertAndProcess is report item
// #8: if a previous delivery inserted the payment_events row and then
// crashed BEFORE processing it (processed_at still NULL), a redelivery must
// resume processing — not silently acknowledge and lose the event forever.
func TestHandleWebhook_ResumesAfterCrashBetweenInsertAndProcess(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, events, _, outbox, gw := newWebhookHarness(p)
	ev := &domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1", ProviderPaymentID: "gw-1",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	}
	gw.verifyFn = verifyOK(ev)

	// Simulate the crash: the row exists (a previous request's Create
	// committed), but it was never marked processed.
	if err := events.Create(context.Background(), &domain.PaymentEvent{
		ID: uuid.New(), Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1",
		ProviderPaymentID: strPtrTest("gw-1"), EventType: &ev.Type, Payload: []byte(`{}`),
		SignatureValid: true, ReceivedAt: time.Now(),
	}); err != nil {
		t.Fatalf("setup Create error = %v", err)
	}

	// Before the fix, this branch returned nil unconditionally on
	// ErrAlreadyExists, and the payment would stay `created` forever.
	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want authorized (the redelivery must have resumed processing)", stored.Status)
	}
	found := false
	for _, ty := range outbox.types() {
		if ty == domain.EventPaymentAuthorized {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want payment.authorized", outbox.types())
	}
	stored2, err := events.GetByProviderEventID(context.Background(), domain.ProviderFreedomPay, "evt-1")
	if err != nil || stored2.ProcessedAt == nil {
		t.Fatalf("event ProcessedAt = %v (err=%v), want set after a successful resume", stored2, err)
	}
}

// TestHandleWebhook_ApplyFailureLeavesEventUnprocessed is report item #9: an
// apply() failure must NOT set processed_at — the event must remain in the
// unprocessed set so a later delivery or the reconciliation worker gets
// another chance, instead of it silently falling out of ClaimUnprocessed's
// scan forever.
func TestHandleWebhook_ApplyFailureLeavesEventUnprocessed(t *testing.T) {
	// `created` payment, told it is `captured`: created -> captured is not an
	// allowed transition (report item #14's out-of-order scenario — captured
	// arriving before authorized).
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	u, repo, events, _, outbox, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1", ProviderPaymentID: "gw-1",
		Type: domain.WebhookPaymentCaptured, Status: domain.PaymentCaptured, SignatureValid: true,
	})

	err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body"), nil)
	if err == nil {
		t.Fatalf("expected an error from an out-of-order transition")
	}
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("error = %v, want ErrInvalidStatus", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, want unchanged created", stored.Status)
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from a failed apply, want 0", len(outbox.types()))
	}
	storedEvent, gerr := events.GetByProviderEventID(context.Background(), domain.ProviderFreedomPay, "evt-1")
	if gerr != nil {
		t.Fatalf("event lookup error = %v", gerr)
	}
	if storedEvent.ProcessedAt != nil {
		t.Fatalf("ProcessedAt = %v, want nil (report item #9: a failed apply must not close the event)", storedEvent.ProcessedAt)
	}
	if storedEvent.ProcessError == nil || *storedEvent.ProcessError == "" {
		t.Fatalf("ProcessError not recorded")
	}
	if storedEvent.PaymentID == nil || *storedEvent.PaymentID != p.ID {
		t.Fatalf("PaymentID = %v, want %s (report item #16: backfilled even on a failed apply)", storedEvent.PaymentID, p.ID)
	}

	// A later redelivery (the authorization webhook finally arrives) must
	// still be able to apply cleanly against the SAME event row's dedup key
	// being irrelevant here — this simulates the reconciliation worker
	// retrying the SAME captured event once the payment is authorized.
	if err := repo.CompareAndSwapStatus(context.Background(), p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("setup authorize error = %v", err)
	}
	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("body-retry"), nil); err != nil {
		t.Fatalf("retried HandleWebhook() error = %v", err)
	}
	stored, _ = repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured after the retry succeeds", stored.Status)
	}
}
