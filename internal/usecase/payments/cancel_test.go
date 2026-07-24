package payments

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// newCancelHarness builds a DepositCancellationUseCase over the hand-written
// fakes, seeded with one AUTHORIZED deposit hold for a booking. The cancel
// deadline is injected directly (fakeCancelDeadlineResolver) — real
// per-restaurant window resolution lives in resolveSettings/bootstrap and is
// unit-tested separately (TestResolveSettings_FreeCancelWindowOverride); here
// the deadline is the seam that lets a test flip the early/late boundary, which
// is exactly what a different per-restaurant window does in production.
func newCancelHarness(t *testing.T, deadline time.Time) (DepositCancellationUseCase, *domain.Payment, *fakePaymentRepo, *fakeLedgerRepo, *fakeGateway) {
	t.Helper()
	bookingID := uuid.New()
	p := testPayment(bookingID, domain.PaymentAuthorized, "gw-dep")
	repo := newFakePaymentRepo(p)
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	booking := &domain.Booking{ID: bookingID, RestaurantID: p.RestaurantID, StartsAt: time.Now().Add(2 * time.Hour)}
	bookings := newFakeBookingReader(booking)
	dl := &fakeCancelDeadlineResolver{deadline: deadline}
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	uc := NewDepositCancellationUseCase(repo, ledger, outbox, resolver, managers, bookings, dl, tx)
	return uc, p, repo, ledger, gw
}

// Early guest cancellation (before the free-cancel deadline) releases the hold
// to the guest: the deposit is VOIDED, no money is captured.
func TestSettleDeposit_EarlyGuestCancel_Voids(t *testing.T) {
	ctx := context.Background()
	// Deadline is one hour in the FUTURE; cancelling now is comfortably before it.
	uc, p, repo, ledger, gw := newCancelHarness(t, time.Now().Add(1*time.Hour))
	now := time.Now()

	got, err := uc.SettleDepositOnCancel(ctx, Actor{}, p.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &now,
	})
	if err != nil {
		t.Fatalf("SettleDepositOnCancel() error = %v", err)
	}
	if got.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided", got.Status)
	}
	if gw.callCount("void") != 1 {
		t.Fatalf("void called %d times, want 1", gw.callCount("void"))
	}
	if gw.callCount("capture") != 0 {
		t.Fatalf("capture called %d times, want 0", gw.callCount("capture"))
	}
	if entries, _ := ledger.ListByPaymentID(ctx, p.ID); len(entries) != 0 {
		t.Fatalf("ledger entries = %d, want 0 (a void moves no money)", len(entries))
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentVoided {
		t.Fatalf("stored status = %s, want voided", stored.Status)
	}
}

// Late guest cancellation (at or after the deadline) forfeits the deposit: the
// hold is CAPTURED, the base goes to the venue and the fee to the platform.
func TestSettleDeposit_LateGuestCancel_Captures(t *testing.T) {
	ctx := context.Background()
	// Deadline is one hour in the PAST; cancelling now is after it.
	uc, p, repo, ledger, gw := newCancelHarness(t, time.Now().Add(-1*time.Hour))
	now := time.Now()

	got, err := uc.SettleDepositOnCancel(ctx, Actor{}, p.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &now,
	})
	if err != nil {
		t.Fatalf("SettleDepositOnCancel() error = %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1", gw.callCount("capture"))
	}
	if gw.callCount("void") != 0 {
		t.Fatalf("void called %d times, want 0", gw.callCount("void"))
	}
	bal, _ := ledger.BalanceByAccount(ctx, p.ID)
	if bal[domain.AccountRestaurant] != -p.BaseAmountMinor {
		t.Fatalf("restaurant balance = %d, want credit of %d", bal[domain.AccountRestaurant], p.BaseAmountMinor)
	}
	if bal[domain.AccountPlatform] != -p.FeeMinor {
		t.Fatalf("platform balance = %d, want credit of %d", bal[domain.AccountPlatform], p.FeeMinor)
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("stored status = %s, want captured", stored.Status)
	}
}

// A no-show is settled EXACTLY like a late cancellation: the deposit is
// captured to the venue, regardless of any deadline.
func TestSettleDeposit_NoShow_Captures(t *testing.T) {
	ctx := context.Background()
	// Deadline in the FUTURE — proves no-show ignores timing and still forfeits.
	uc, p, _, _, gw := newCancelHarness(t, time.Now().Add(5*time.Hour))

	got, err := uc.SettleDepositOnCancel(ctx, staffActor, p.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerNoShow,
	})
	if err != nil {
		t.Fatalf("SettleDepositOnCancel() error = %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1", gw.callCount("capture"))
	}
}

// A venue-caused cancellation releases the hold to the guest (void), whatever
// the timing.
func TestSettleDeposit_VenueCancel_Voids(t *testing.T) {
	ctx := context.Background()
	uc, p, _, _, gw := newCancelHarness(t, time.Now().Add(-5*time.Hour)) // late window, still voids
	got, err := uc.SettleDepositOnCancel(ctx, staffActor, p.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerVenueCancel,
	})
	if err != nil {
		t.Fatalf("SettleDepositOnCancel() error = %v", err)
	}
	if got.Status != domain.PaymentVoided {
		t.Fatalf("status = %s, want voided", got.Status)
	}
	if gw.callCount("void") != 1 || gw.callCount("capture") != 0 {
		t.Fatalf("void=%d capture=%d, want void=1 capture=0", gw.callCount("void"), gw.callCount("capture"))
	}
}

// The per-restaurant free-cancel window is what decides early vs late: the same
// guest cancellation, at the same wall-clock moment, VOIDS under a wide window
// (deadline still ahead) but CAPTURES under a narrow one (deadline already
// passed). This is exactly a 300-minute venue vs a 60-minute venue flipping the
// boundary.
func TestSettleDeposit_PerRestaurantWindowFlipsBoundary(t *testing.T) {
	ctx := context.Background()
	cancelAt := time.Now()

	// Wide window → deadline is still in the future → cancel is "in time" → void.
	wideUC, wideP, _, _, wideGW := newCancelHarness(t, cancelAt.Add(30*time.Minute))
	if _, err := wideUC.SettleDepositOnCancel(ctx, Actor{}, wideP.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &cancelAt,
	}); err != nil {
		t.Fatalf("wide-window settle error = %v", err)
	}
	if wideGW.callCount("void") != 1 || wideGW.callCount("capture") != 0 {
		t.Fatalf("wide window: void=%d capture=%d, want void=1 capture=0", wideGW.callCount("void"), wideGW.callCount("capture"))
	}

	// Narrow window → deadline already passed → same cancel is "late" → capture.
	narrowUC, narrowP, _, _, narrowGW := newCancelHarness(t, cancelAt.Add(-30*time.Minute))
	if _, err := narrowUC.SettleDepositOnCancel(ctx, Actor{}, narrowP.BookingID, DepositCancelInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &cancelAt,
	}); err != nil {
		t.Fatalf("narrow-window settle error = %v", err)
	}
	if narrowGW.callCount("capture") != 1 || narrowGW.callCount("void") != 0 {
		t.Fatalf("narrow window: void=%d capture=%d, want void=0 capture=1", narrowGW.callCount("void"), narrowGW.callCount("capture"))
	}
}

// A double cancellation must not move money twice: the second call finds the
// deposit already captured and is a no-op.
func TestSettleDeposit_DoubleCancel_Idempotent(t *testing.T) {
	ctx := context.Background()
	uc, p, _, _, gw := newCancelHarness(t, time.Now().Add(-1*time.Hour))
	now := time.Now()
	in := DepositCancelInput{Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &now}

	if _, err := uc.SettleDepositOnCancel(ctx, Actor{}, p.BookingID, in); err != nil {
		t.Fatalf("first settle error = %v", err)
	}
	got, err := uc.SettleDepositOnCancel(ctx, Actor{}, p.BookingID, in)
	if err != nil {
		t.Fatalf("second settle error = %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status after second settle = %s, want captured", got.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times across two settles, want 1", gw.callCount("capture"))
	}
}

// A manual late cancellation racing the no-show worker: both want to CAPTURE
// the same held deposit concurrently. Only one may reach the acquirer.
func TestSettleDeposit_CancelRacingNoShow_NoDoubleMove(t *testing.T) {
	ctx := context.Background()
	uc, p, repo, _, gw := newCancelHarness(t, time.Now().Add(-1*time.Hour))
	gw.captureDelay = 20 * time.Millisecond // force the two goroutines to actually overlap
	now := time.Now()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	inputs := []DepositCancelInput{
		{Trigger: domain.RefundTriggerGuestCancel, CancelledAt: &now},
		{Trigger: domain.RefundTriggerNoShow},
	}
	actors := []Actor{{}, staffActor}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = uc.SettleDepositOnCancel(ctx, actors[i], p.BookingID, inputs[i])
		}(i)
	}
	wg.Wait()

	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times under the race, want exactly 1", gw.callCount("capture"))
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("final status = %s, want captured", stored.Status)
	}
}

// A booking that never took a deposit has no live payment: settling its
// cancellation is a cheap no-op, so the cancel/no-show hook can call it
// unconditionally.
func TestSettleDeposit_NoLivePayment_NoOp(t *testing.T) {
	ctx := context.Background()
	uc, _, _, _, gw := newCancelHarness(t, time.Now())
	got, err := uc.SettleDepositOnCancel(ctx, Actor{}, uuid.New() /* unknown booking */, DepositCancelInput{
		Trigger: domain.RefundTriggerGuestCancel,
	})
	if err != nil {
		t.Fatalf("SettleDepositOnCancel() error = %v", err)
	}
	if got != nil {
		t.Fatalf("payment = %v, want nil (no deposit to settle)", got)
	}
	if gw.callCount("void")+gw.callCount("capture") != 0 {
		t.Fatalf("gateway was called for a booking with no payment")
	}
}

// resolveSettings maps the per-restaurant free_cancel_window_minutes override
// onto PaymentSettings.FreeCancelWindow, falling back to the global default.
func TestResolveSettings_FreeCancelWindowOverride(t *testing.T) {
	cfg := Config{}.withDefaults()
	if got := resolveSettings(domain.PaymentSettingsOverride{}, cfg).FreeCancelWindow; got != 120*time.Minute {
		t.Fatalf("default window = %s, want 120m", got)
	}
	win := 60
	if got := resolveSettings(domain.PaymentSettingsOverride{FreeCancelWindowMinutes: &win}, cfg).FreeCancelWindow; got != 60*time.Minute {
		t.Fatalf("override window = %s, want 60m", got)
	}
}

// A PRE-ORDER is captured immediately at payment time (the kitchen prepares the
// food): the moment its hold is authorized it is charged, ending in `captured`
// without waiting for seating. A DEPOSIT, by contrast, stays authorized (see
// the settle-on-cancel tests above).
func TestPreorderCapturedImmediatelyOnAuthorization(t *testing.T) {
	ctx := context.Background()
	pre := testPayment(uuid.New(), domain.PaymentCreated, "gw-pre")
	pre.Purpose = domain.PurposePreorder
	u, repo, _, ledger, _, gw := newWebhookHarness(pre)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-pre", ProviderPaymentID: "gw-pre",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})

	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(ctx, pre.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("pre-order status = %s, want captured immediately", stored.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1", gw.callCount("capture"))
	}
	bal, _ := ledger.BalanceByAccount(ctx, pre.ID)
	if bal[domain.AccountRestaurant] != -pre.BaseAmountMinor {
		t.Fatalf("restaurant balance = %d, want credit of %d", bal[domain.AccountRestaurant], pre.BaseAmountMinor)
	}
}

// A DEPOSIT is NOT captured on authorization — it stays a hold until a late
// cancellation / no-show forfeits it.
func TestDepositNotCapturedOnAuthorization(t *testing.T) {
	ctx := context.Background()
	dep := testPayment(uuid.New(), domain.PaymentCreated, "gw-dep2") // Purpose defaults to deposit
	u, repo, _, _, _, gw := newWebhookHarness(dep)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-dep", ProviderPaymentID: "gw-dep2",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})

	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(ctx, dep.ID)
	if stored.Status != domain.PaymentAuthorized {
		t.Fatalf("deposit status = %s, want authorized (held, not captured)", stored.Status)
	}
	if gw.callCount("capture") != 0 {
		t.Fatalf("capture called %d times for a deposit, want 0", gw.callCount("capture"))
	}
}
