package payments

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// reconcilerHarness wires a Reconciler over the same fakes the rest of this
// package uses, with a frozen, injectable clock (mirrors bookings.Worker's
// test harness).
type reconcilerHarness struct {
	r        *Reconciler
	payments *fakePaymentRepo
	refunds  *fakeRefundRepo
	ledger   *fakeLedgerRepo
	outbox   *fakePaymentOutbox
	gw       *fakeGateway
	now      time.Time
}

// reconcilerHarnessNow is the frozen instant every reconciler test builds its
// fixtures relative to. Any fixture that instead anchors itself to the real
// wall clock (time.Now()) silently stops matching the reconciler's own,
// injected clock once enough wall-clock time has passed between when the
// fixture literal was written and when the test actually runs - a payment
// "changed 20 minutes ago" relative to time.Now() is not "changed 20 minutes
// ago" relative to h.now() unless they are the same instant.
var reconcilerHarnessNow = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func newReconcilerHarness(t *testing.T, cfg ReconcilerConfig, payments []*domain.Payment, refunds []*domain.PaymentRefund) *reconcilerHarness {
	t.Helper()
	pr := newFakePaymentRepo(payments...)
	rr := newFakeRefundRepo()
	for _, rf := range refunds {
		if err := rr.Create(context.Background(), rf); err != nil {
			t.Fatalf("seed refund: %v", err)
		}
	}
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: pr, ledger: ledger, outbox: outbox, refunds: rr}

	cfg.ProviderMinGap = 0 // no real sleeping in unit tests
	h := &reconcilerHarness{
		payments: pr, refunds: rr, ledger: ledger, outbox: outbox, gw: gw,
		now: reconcilerHarnessNow,
	}
	h.r = NewReconciler(pr, rr, ledger, outbox, resolver, tx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.r.now = func() time.Time { return h.now }
	return h
}

// stuckCapturingPayment builds an authorized-turned-capturing payment whose
// StatusChangedAt is changedAgo before the harness clock.
func stuckCapturingPayment(bookingID uuid.UUID, providerPaymentID string, changedAgo time.Duration, now time.Time) *domain.Payment {
	p := testPayment(bookingID, domain.PaymentCapturing, providerPaymentID)
	p.StatusChangedAt = now.Add(-changedAgo)
	return p
}

func stuckVoidingPayment(bookingID uuid.UUID, providerPaymentID string, changedAgo time.Duration, now time.Time) *domain.Payment {
	p := testPayment(bookingID, domain.PaymentVoiding, providerPaymentID)
	p.StatusChangedAt = now.Add(-changedAgo)
	return p
}

// ---------------------------------------------------------------------------
// capturing
// ---------------------------------------------------------------------------

// Stuck capturing, acquirer says captured → finished with ledger entries,
// and a second Tick (the acquirer already resolved, or a webhook got there
// first) does not duplicate anything.
func TestReconciler_StuckCapturing_AcquirerCaptured_FinishesAndIsIdempotent(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", 20*time.Minute, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentCaptured, Amount: p.Total()}

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.Resolved != 1 || res.StillUnknown != 0 {
		t.Fatalf("got %+v, want 1 resolved", res)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	entries, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		t.Fatalf("ledger does not balance: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ledger entries = %d, want 3 (guest debit, restaurant credit, platform credit)", len(entries))
	}

	// Second tick: nothing left to claim (status moved to captured), no
	// duplicate ledger batch.
	if _, err := h.r.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	entries2, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)
	if len(entries2) != len(entries) {
		t.Fatalf("second tick duplicated ledger entries: got %d, want %d", len(entries2), len(entries))
	}
}

// Stuck capturing, acquirer says the hold is still just authorized (the
// capture attempt never actually landed) → released back to authorized.
func TestReconciler_StuckCapturing_AcquirerNotCaptured_ReturnsToAuthorized(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", 20*time.Minute, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentAuthorized}

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.Resolved != 1 {
		t.Fatalf("got %+v, want 1 resolved", res)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want authorized", got.Status)
	}
	if entries, _ := h.ledger.ListByPaymentID(context.Background(), p.ID); len(entries) != 0 {
		t.Fatalf("no money moved, but %d ledger entries were written", len(entries))
	}
}

// Unknown acquirer answer (transport error) → status untouched, attempt
// counter grows.
func TestReconciler_StuckCapturing_UnknownOutcome_LeavesStatusAloneBumpsAttempts(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 5}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", 20*time.Minute, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getErr = errTimeout

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.StillUnknown != 1 || res.Resolved != 0 {
		t.Fatalf("got %+v, want 1 still unknown", res)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCapturing {
		t.Fatalf("status = %s, want unchanged capturing", got.Status)
	}
	if got.ReconcileAttempts != 1 {
		t.Fatalf("reconcile attempts = %d, want 1", got.ReconcileAttempts)
	}
	if got.NeedsManualReview {
		t.Fatal("flagged for manual review after only 1 attempt")
	}
}

// A payment that has not yet crossed StuckAfter must not be touched at all.
func TestReconciler_NotStuckBeforeThreshold_Untouched(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", 2*time.Minute, h.now) // younger than StuckAfter
	h.payments.byID[p.ID] = p
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentCaptured}

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.StuckFound != 0 {
		t.Fatalf("got %+v, want nothing claimed (below threshold)", res)
	}
	if h.gw.callCount("get") != 0 {
		t.Fatalf("acquirer called %d times, want 0 — payment is not stuck yet", h.gw.callCount("get"))
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCapturing {
		t.Fatalf("status = %s, want unchanged", got.Status)
	}
}

// N consecutive unknown outcomes → flagged NeedsManualReview and the worker
// stops calling the acquirer for it.
func TestReconciler_MaxAttemptsExceeded_FlaggedForManualReview(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: time.Minute, BatchSize: 10, MaxAttempts: 2}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", time.Hour, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getErr = errTimeout

	// First attempt: attempts=1, not yet flagged.
	if _, err := h.r.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.NeedsManualReview {
		t.Fatal("flagged too early")
	}

	// Backoff after one failed attempt would normally skip a quick retry;
	// push the clock far enough forward that the second attempt is due.
	h.now = h.now.Add(2 * time.Hour)

	res2, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res2.ManualReview != 1 {
		t.Fatalf("got %+v, want manual_review=1 on the tick that crosses the threshold", res2)
	}
	got, _ = h.payments.GetByID(context.Background(), p.ID)
	if !got.NeedsManualReview {
		t.Fatal("not flagged after reaching MaxAttempts")
	}
	if got.ReconcileAttempts != 2 {
		t.Fatalf("reconcile attempts = %d, want 2", got.ReconcileAttempts)
	}

	callsBefore := h.gw.callCount("get")
	h.now = h.now.Add(2 * time.Hour)
	if _, err := h.r.Tick(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if h.gw.callCount("get") != callsBefore {
		t.Fatalf("acquirer called again after manual-review flag was set (avalanche guard broken): before=%d after=%d",
			callsBefore, h.gw.callCount("get"))
	}
}

// ---------------------------------------------------------------------------
// voiding (symmetric to capturing, one representative case)
// ---------------------------------------------------------------------------

func TestReconciler_StuckVoiding_AcquirerVoided_Finishes(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckVoidingPayment(bookingID, "gw-1", 20*time.Minute, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentVoided}

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.Resolved != 1 {
		t.Fatalf("got %+v, want 1 resolved", res)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided", got.Status)
	}
	found := false
	for _, ty := range h.outbox.types() {
		if ty == domain.EventPaymentVoided {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want payment.voided", h.outbox.types())
	}
}

// ---------------------------------------------------------------------------
// refunds stuck in_flight / pending
// ---------------------------------------------------------------------------

// A refund left `pending` after a previous timeout: the acquirer now confirms
// it went through → refund + ledger finished, both agree.
func TestReconciler_RefundPending_AcquirerSucceeded_Finishes(t *testing.T) {
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	p := testPayment(uuid.New(), domain.PaymentCaptured, "gw-1")
	// A guest-cancel-before-deadline settlement: total 1_035_000, guest gets
	// back 1_024_600 (1% acquiring withheld = 10_400).
	p.AmountMinor, p.BaseAmountMinor, p.FeeMinor = 1_035_000, 1_000_000, 35_000
	// Anchor every timestamp to the same frozen clock the harness injects into
	// the reconciler (reconcilerHarnessNow), not to the real wall clock: the
	// reconciler compares StatusChangedAt against its own now(), never
	// time.Now(), so a fixture built off time.Now() drifts out of the
	// "changed StuckAfter ago" window as soon as real time and the frozen
	// clock diverge (see the bug this test used to have).
	now := reconcilerHarnessNow
	settledAt := now
	trig := domain.RefundTriggerGuestCancel
	key := "settle-1"
	p.SettledAt, p.SettledTrigger, p.SettlementIdempotencyKey = &settledAt, &trig, &key

	changedAt := now.Add(-20 * time.Minute)
	rf := &domain.PaymentRefund{
		ID: uuid.New(), PaymentID: p.ID, AmountMinor: 1_024_600, Currency: domain.CurrencyKZT,
		Status: domain.RefundPending, IdempotencyKey: key, CreatedAt: changedAt, UpdatedAt: changedAt,
		StatusChangedAt: changedAt,
	}

	h := newReconcilerHarness(t, cfg, []*domain.Payment{p}, []*domain.PaymentRefund{rf})
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentRefunded, Amount: p.Total()}

	res, err := h.r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if res.Resolved != 1 {
		t.Fatalf("got %+v, want 1 resolved", res)
	}
	gotRefund, _ := h.refunds.GetByID(context.Background(), rf.ID)
	if gotRefund.Status != domain.RefundSucceeded {
		t.Fatalf("refund status = %s, want succeeded", gotRefund.Status)
	}
	gotPayment, _ := h.payments.GetByID(context.Background(), p.ID)
	if gotPayment.Status != domain.PaymentRefunded {
		t.Fatalf("payment status = %s, want refunded", gotPayment.Status)
	}
	entries, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		t.Fatalf("ledger does not balance: %v", err)
	}
	bal, _ := h.ledger.BalanceByAccount(context.Background(), p.ID)
	if bal[domain.AccountGuest] != -rf.AmountMinor {
		t.Fatalf("guest balance = %d, want credit of %d", bal[domain.AccountGuest], rf.AmountMinor)
	}
	if bal[domain.AccountAcquirer] != -(p.AmountMinor - rf.AmountMinor) {
		t.Fatalf("acquirer balance = %d, want credit of %d", bal[domain.AccountAcquirer], p.AmountMinor-rf.AmountMinor)
	}
}

// ---------------------------------------------------------------------------
// race: the reconciler and a webhook resolve the same capturing payment at
// the same time. Only one write wins; there is exactly one ledger batch.
// ---------------------------------------------------------------------------

func TestReconciler_RaceWithWebhook_SingleLedgerBatch(t *testing.T) {
	bookingID := uuid.New()
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	p := stuckCapturingPayment(bookingID, "gw-1", 20*time.Minute, h.now)
	h.payments.byID[p.ID] = p
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentCaptured, Amount: p.Total()}

	// Simulate a webhook that already applied the SAME capture just before
	// the reconciler's own CAS runs, by driving it through the exact same
	// state-machine code (webhookUseCase.apply) the reconciler itself calls.
	events := newFakeEventRepo()
	webhookUC := NewWebhookUseCase(h.payments, events, h.ledger, h.outbox, newFakeGatewayResolver(h.gw), &fakeTx{payments: h.payments, ledger: h.ledger, outbox: h.outbox})
	verifyFn := func([]byte, map[string]string) (*domain.WebhookEvent, error) {
		return &domain.WebhookEvent{
			Provider: p.Provider, ProviderEventID: "evt-1", ProviderPaymentID: *p.ProviderPaymentID,
			Type: domain.WebhookPaymentCaptured, Status: domain.PaymentCaptured, Amount: p.Total(),
			SignatureValid: true,
		}, nil
	}
	h.gw.verifyFn = verifyFn
	if err := webhookUC.HandleWebhook(context.Background(), p.Provider, []byte("body"), nil); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("webhook did not capture: status = %s", got.Status)
	}
	entriesAfterWebhook, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)

	// Now the reconciler picks up the SAME payment id in its own claim — it
	// only sees it because its StatusChangedAt was recorded before the
	// webhook ran; in production the CAS below is what actually decides who
	// wins, exercised directly here since ClaimStale no longer selects an
	// already-captured row.
	staleClaim := *got
	staleClaim.Status = domain.PaymentCapturing // pretend the reconciler read it before the webhook's CAS committed
	if err := h.r.finishCapture(context.Background(), &staleClaim, h.now); err != nil {
		t.Fatalf("finishCapture on an already-captured payment must be treated as success: %v", err)
	}

	entriesAfter, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)
	if len(entriesAfter) != len(entriesAfterWebhook) {
		t.Fatalf("reconciler duplicated the ledger batch: webhook wrote %d entries, after reconciler %d",
			len(entriesAfterWebhook), len(entriesAfter))
	}
}

// errTimeout stands in for a network timeout / 5xx the acquirer adapter would
// wrap as domain.ErrProviderOutcomeUnknown in production; the reconciler
// treats any Get() error the same way, so a plain sentinel is enough here.
var errTimeout = errAcquirerTimeout{}

type errAcquirerTimeout struct{}

func (errAcquirerTimeout) Error() string { return "acquirer: request timed out" }
