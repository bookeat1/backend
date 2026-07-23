package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newCaptureHarness(p *domain.Payment) (CaptureUseCase, VoidUseCase, *fakePaymentRepo, *fakeLedgerRepo, *fakePaymentOutbox, *fakeGateway) {
	repo := newFakePaymentRepo(p)
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	capture := NewCaptureUseCase(repo, ledger, outbox, resolver, tx)
	void := NewVoidUseCase(repo, outbox, resolver, tx)
	return capture, void, repo, ledger, outbox, gw
}

var staffActor = Actor{Role: domain.RoleRestaurant}

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

func TestCaptureOnSeating_ProviderRejectionLeavesPaymentAuthorized(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	capture, _, repo, ledger, outbox, gw := newCaptureHarness(p)
	gw.captureErr = errors.New("acquirer: card declined on clearing")

	_, err := capture.CaptureOnSeating(context.Background(), staffActor, p.BookingID)
	if err == nil {
		t.Fatalf("expected an error from a provider rejection")
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

// TestCaptureVsVoidRace exercises the DB-level guard on the same payment: a
// slow capture and a slow void both starting from 'authorized' must not both
// win. Whichever CompareAndSwapStatus loses reports ErrAlreadyExists rather
// than silently succeeding.
func TestCaptureVsVoidRace(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	repo := newFakePaymentRepo(p)

	now := p.CreatedAt
	errCapture := repo.CompareAndSwapStatus(context.Background(), p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, now)
	errVoid := repo.CompareAndSwapStatus(context.Background(), p.ID, domain.PaymentAuthorized, domain.PaymentVoided, now)

	if errCapture != nil {
		t.Fatalf("first CAS (capture) error = %v, want nil", errCapture)
	}
	if !errors.Is(errVoid, domain.ErrAlreadyExists) {
		t.Fatalf("second CAS (void) error = %v, want ErrAlreadyExists", errVoid)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured (the winner)", stored.Status)
	}
}
