package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newCaptureHarness(p *domain.Payment) (CaptureUseCase, VoidUseCase, *fakePaymentRepo, *fakeLedgerRepo, *fakePaymentOutbox, *fakeGateway) {
	repo := newFakePaymentRepo(p)
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	capture := NewCaptureUseCase(repo, ledger, outbox, resolver, managers, tx)
	void := NewVoidUseCase(repo, outbox, resolver, managers, tx)
	return capture, void, repo, ledger, outbox, gw
}

var staffUserID = uuid.New()
var staffActor = Actor{UserID: &staffUserID, Role: domain.RoleRestaurant}

func TestCaptureOnSeating_HappyPath(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, ledger, outbox, gw := newCaptureHarness(p)

	got, err := capture.CaptureOnSeating(context.Background(), staffActor, p.BookingID)
	if err != nil {
		t.Fatalf("CaptureOnSeating() error = %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1", gw.callCount("capture"))
	}
	entries, _ := ledger.ListByPaymentID(context.Background(), p.ID)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		t.Fatalf("ledger does not balance: %v", err)
	}
	bal, _ := ledger.BalanceByAccount(context.Background(), p.ID)
	if bal[domain.AccountRestaurant] != -p.BaseAmountMinor {
		t.Fatalf("restaurant balance = %d, want credit of %d", bal[domain.AccountRestaurant], p.BaseAmountMinor)
	}
	if bal[domain.AccountPlatform] != -p.FeeMinor {
		t.Fatalf("platform balance = %d, want credit of %d", bal[domain.AccountPlatform], p.FeeMinor)
	}
	if bal[domain.AccountGuest] != p.AmountMinor {
		t.Fatalf("guest balance = %d, want debit of %d", bal[domain.AccountGuest], p.AmountMinor)
	}
	found := false
	for _, ty := range outbox.types() {
		if ty == domain.EventPaymentCaptured {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want payment.captured", outbox.types())
	}
	_ = repo
}

func TestCaptureOnSeating_IdempotentNoSecondCapture(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, _, _, _, gw := newCaptureHarness(p)
	ctx := context.Background()

	if _, err := capture.CaptureOnSeating(ctx, staffActor, p.BookingID); err != nil {
		t.Fatalf("first capture error = %v", err)
	}
	if _, err := capture.CaptureOnSeating(ctx, staffActor, p.BookingID); err != nil {
		t.Fatalf("second capture error = %v", err)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times across 2 calls, want 1 (unconfirmed idempotency at the acquirer, see sandbox checklist #7)", gw.callCount("capture"))
	}
}

func TestCaptureOnSeating_RejectsNonAuthorized(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-1")
	capture, _, _, _, _, _ := newCaptureHarness(p)

	_, err := capture.CaptureOnSeating(context.Background(), staffActor, p.BookingID)
	if !errors.Is(err, domain.ErrNotFound) {
		// GetLiveByBookingID only returns authorized/captured; a 'created'
		// payment is not "live" yet, so this is correctly a not-found, not an
		// invalid-status error.
		t.Fatalf("error = %v, want ErrNotFound (payment not live)", err)
	}
}

func TestCaptureOnSeating_RejectsGuestActor(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, _, _, _, _ := newCaptureHarness(p)

	_, err := capture.CaptureOnSeating(context.Background(), Actor{Role: domain.RoleUser}, p.BookingID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

// TestCaptureOnSeating_ProviderRejectionLeavesPaymentAuthorized covers a
// DEFINITE decline (domain.ErrProviderDeclined) — the hold is unchanged and
// must be released back to `authorized` so a retry (or a Void) can proceed.
func TestCaptureOnSeating_ProviderRejectionLeavesPaymentAuthorized(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, ledger, outbox, gw := newCaptureHarness(p)
	gw.captureErr = fmt.Errorf("acquirer: card declined on clearing: %w", domain.ErrProviderDeclined)

	_, err := capture.CaptureOnSeating(context.Background(), staffActor, p.BookingID)
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("error = %v, want ErrProviderDeclined", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s, want unchanged authorized", stored.Status)
	}
	if len(ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries from a failed capture, want 0", len(ledger.entries))
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from a failed capture, want 0", len(outbox.types()))
	}
}

// TestCaptureOnSeating_UnknownOutcomeStaysCapturingAndForbidsRetry is report
// item #1 (second review): a timeout / 5xx / malformed answer from the
// acquirer is NOT the same fact as a definite decline — the capture may have
// actually cleared. The payment must be left in `capturing`, and a second
// CaptureOnSeating call for the same booking must be refused rather than
// calling gw.Capture again (which would risk a real second charge).
func TestCaptureOnSeating_UnknownOutcomeStaysCapturingAndForbidsRetry(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, ledger, outbox, gw := newCaptureHarness(p)
	gw.captureErr = fmt.Errorf("acquirer: request timed out: %w", domain.ErrProviderOutcomeUnknown)
	ctx := context.Background()

	_, err := capture.CaptureOnSeating(ctx, staffActor, p.BookingID)
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("first call error = %v, want ErrProviderOutcomeUnknown", err)
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCapturing {
		t.Fatalf("status = %s, want capturing (parked pending reconciliation)", stored.Status)
	}
	if len(ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries from an unresolved capture, want 0", len(ledger.entries))
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from an unresolved capture, want 0", len(outbox.types()))
	}

	// A second attempt (staff retrying, thinking the first one "failed")
	// must NOT call the acquirer again.
	_, err = capture.CaptureOnSeating(ctx, staffActor, p.BookingID)
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("second call error = %v, want ErrInvalidStatus (payment is capturing, cannot capture)", err)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times across 2 attempts, want 1 (must not retry an unknown outcome)", gw.callCount("capture"))
	}
}

func TestVoidOnRejection_HappyPath(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	_, void, repo, _, outbox, gw := newCaptureHarness(p)

	got, err := void.VoidOnRejection(context.Background(), staffActor, p.BookingID, "table not available")
	if err != nil {
		t.Fatalf("VoidOnRejection() error = %v", err)
	}
	if got.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided", got.Status)
	}
	if gw.callCount("void") != 1 {
		t.Fatalf("void called %d times, want 1", gw.callCount("void"))
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.VoidedAt == nil {
		t.Fatalf("VoidedAt not set")
	}
	found := false
	for _, ty := range outbox.types() {
		if ty == domain.EventPaymentVoided {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbox events = %v, want payment.voided", outbox.types())
	}
}

// TestCaptureOnSeating_ConcurrentCallsOnlyOneCallsGateway is report item #5:
// two concurrent CaptureOnSeating calls for the SAME booking (a double click,
// a retried request) must not both call gw.Capture — the loser must lose the
// `authorized -> capturing` CAS before ever reaching the acquirer.
func TestCaptureOnSeating_ConcurrentCallsOnlyOneCallsGateway(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, _, _, gw := newCaptureHarness(p)
	gw.captureDelay = 20 * time.Millisecond
	ctx := context.Background()

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = capture.CaptureOnSeating(ctx, staffActor, p.BookingID)
		}(i)
	}
	start.Done()
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d across %d concurrent calls, want exactly 1: errs=%v", successes, n, errs)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1 (the CAS claim must stop every loser before the acquirer call)", gw.callCount("capture"))
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", stored.Status)
	}
}

// TestCaptureOnSeating_LostClaimButAlreadyCapturedIsSuccess is report item
// #6: losing the `authorized -> capturing` CAS because a webhook already
// completed the capture must be reported as success, not as a false-alarm
// conflict that would provoke a manual retry (and a second real capture).
func TestCaptureOnSeating_LostClaimButAlreadyCapturedIsSuccess(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, _, _, gw := newCaptureHarness(p)
	ctx := context.Background()

	// Simulate a webhook that already finished the capture between this
	// staff request reading the payment and it attempting to claim the CAS.
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, time.Now()); err != nil {
		t.Fatalf("setup CAS error = %v", err)
	}

	got, err := capture.CaptureOnSeating(ctx, staffActor, p.BookingID)
	if err != nil {
		t.Fatalf("CaptureOnSeating() error = %v, want nil (already captured by the webhook)", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	if gw.callCount("capture") != 0 {
		t.Fatalf("capture called %d times, want 0 (must not re-call the acquirer)", gw.callCount("capture"))
	}
}

// TestCaptureOnSeating_CrossTenantStaffIsRejected is report item #13: staff
// of a DIFFERENT restaurant must not be able to capture this payment's hold
// merely by knowing its booking id.
func TestCaptureOnSeating_CrossTenantStaffIsRejected(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	repo := newFakePaymentRepo(p)
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	strangerStaff := uuid.New()
	managers := &fakeManagerChecker{managed: map[uuid.UUID]map[uuid.UUID]bool{}, allowAllByDefault: false}
	managers.set(strangerStaff, p.RestaurantID, false)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	capture := NewCaptureUseCase(repo, ledger, outbox, resolver, managers, tx)

	_, err := capture.CaptureOnSeating(context.Background(), Actor{UserID: &strangerStaff, Role: domain.RoleRestaurant}, p.BookingID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden (staff of a different restaurant)", err)
	}
	if gw.callCount("capture") != 0 {
		t.Fatalf("capture called %d times, want 0", gw.callCount("capture"))
	}
}

// TestCaptureVsVoidRace exercises the real usecases end to end (not the raw
// CAS in isolation — the QA report flagged the previous version of this test
// for calling repo.CompareAndSwapStatus directly, which never goes through
// CaptureOnSeating/VoidOnRejection and so cannot catch a bug in either one,
// e.g. non-blocking item #1's missing claim in VoidOnRejection): a concurrent
// CaptureOnSeating and VoidOnRejection for the SAME booking, both starting
// from 'authorized', must not both win — exactly one of them may reach its
// acquirer call, and the other must lose the CAS claim and report a conflict
// instead of silently doing nothing or double-acting.
func TestCaptureVsVoidRace(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, void, repo, _, _, gw := newCaptureHarness(p)
	gw.captureDelay = 20 * time.Millisecond
	gw.voidDelay = 20 * time.Millisecond
	ctx := context.Background()

	var captureErr, voidErr error
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		start.Wait()
		_, captureErr = capture.CaptureOnSeating(ctx, staffActor, p.BookingID)
	}()
	go func() {
		defer wg.Done()
		start.Wait()
		_, voidErr = void.VoidOnRejection(ctx, staffActor, p.BookingID, "table not available")
	}()
	start.Done()
	wg.Wait()

	captureWon := captureErr == nil
	voidWon := voidErr == nil
	if captureWon == voidWon {
		t.Fatalf("exactly one of capture/void must win, got captureErr=%v voidErr=%v", captureErr, voidErr)
	}
	if captureWon && !errors.Is(voidErr, domain.ErrInvalidStatus) && !errors.Is(voidErr, domain.ErrAlreadyExists) {
		t.Fatalf("void loser error = %v, want ErrInvalidStatus or ErrAlreadyExists", voidErr)
	}
	if voidWon && !errors.Is(captureErr, domain.ErrInvalidStatus) && !errors.Is(captureErr, domain.ErrAlreadyExists) {
		t.Fatalf("capture loser error = %v, want ErrInvalidStatus or ErrAlreadyExists", captureErr)
	}
	if gw.callCount("capture") > 1 {
		t.Fatalf("capture called %d times, want at most 1", gw.callCount("capture"))
	}
	if gw.callCount("void") > 1 {
		t.Fatalf("void called %d times, want at most 1", gw.callCount("void"))
	}

	stored, _ := repo.GetByID(ctx, p.ID)
	if captureWon && stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured (the winner)", stored.Status)
	}
	if voidWon && stored.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided (the winner)", stored.Status)
	}
}

// TestVoidOnRejection_ConcurrentCallsOnlyOneCallsGateway is the void-side
// analogue of TestCaptureOnSeating_ConcurrentCallsOnlyOneCallsGateway
// (non-blocking item #1, second review): two concurrent VoidOnRejection
// calls for the SAME booking must not both call gw.Void — only the winner of
// the `authorized -> voiding` CAS may reach the acquirer.
func TestVoidOnRejection_ConcurrentCallsOnlyOneCallsGateway(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	_, void, repo, _, _, gw := newCaptureHarness(p)
	gw.voidDelay = 20 * time.Millisecond
	ctx := context.Background()

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = void.VoidOnRejection(ctx, staffActor, p.BookingID, "table not available")
		}(i)
	}
	start.Done()
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d across %d concurrent calls, want exactly 1: errs=%v", successes, n, errs)
	}
	if gw.callCount("void") != 1 {
		t.Fatalf("void called %d times, want 1 (the CAS claim must stop every loser before the acquirer call)", gw.callCount("void"))
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided", stored.Status)
	}
}
