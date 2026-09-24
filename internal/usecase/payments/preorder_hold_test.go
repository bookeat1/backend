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

// Owner decisions 2026-09-24: a pre-order is a two-stage hold, captured when the
// venue confirms, voided when the booking closes before that.

func heldPreorder(bookingID uuid.UUID) *domain.Payment {
	p := testPayment(bookingID, domain.PaymentAuthorized, "gw-hold")
	p.Purpose = domain.PurposePreorder
	p.RequiresConfirmation = true
	return p
}

type holdHarness struct {
	uc      DepositCancellationUseCase
	repo    *fakePaymentRepo
	ledger  *fakeLedgerRepo
	gw      *fakeGateway
	refunds *fakeRefundRepo
}

func newHoldHarness(t *testing.T, p *domain.Payment, confirmed bool) *holdHarness {
	t.Helper()
	repo := newFakePaymentRepo(p)
	refunds := newFakeRefundRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	b := &domain.Booking{ID: p.BookingID, RestaurantID: p.RestaurantID, StartsAt: time.Now().Add(2 * time.Hour)}
	if confirmed {
		now := time.Now()
		b.Status, b.ConfirmedAt = domain.BookingConfirmed, &now
	}
	bookings := newFakeBookingReader(b)
	dl := &fakeCancelDeadlineResolver{deadline: time.Now().Add(-time.Hour)} // free-cancel window already over
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox, refunds: refunds}
	refundUC := NewRefundUseCase(repo, refunds, ledger, outbox, resolver, managers, bookings, dl, tx, Config{}.withDefaults())
	uc := NewDepositCancellationUseCase(repo, ledger, outbox, resolver, managers, bookings, dl, refundUC, tx)
	return &holdHarness{uc: uc, repo: repo, ledger: ledger, gw: gw, refunds: refunds}
}

// A booking the venue never confirmed voids the hold for EVERY trigger — even a
// guest cancelling after the free-cancel window: nothing was accepted.
func TestSettleHeldPreorder_NeverConfirmedIsVoided(t *testing.T) {
	for _, trig := range []domain.RefundTrigger{
		domain.RefundTriggerGuestCancel, domain.RefundTriggerVenueCancel,
	} {
		t.Run(string(trig), func(t *testing.T) {
			ctx := context.Background()
			p := heldPreorder(uuid.New())
			h := newHoldHarness(t, p, false)
			now := time.Now()
			got, err := h.uc.SettleDepositOnCancel(ctx, systemActor, p.BookingID, DepositCancelInput{Trigger: trig, CancelledAt: &now})
			if err != nil {
				t.Fatalf("settle: %v", err)
			}
			if got.Status != domain.PaymentVoided {
				t.Fatalf("status = %s, want voided", got.Status)
			}
			if h.gw.callCount("void") != 1 || h.gw.callCount("capture") != 0 || h.gw.callCount("refund") != 0 {
				t.Fatalf("void/capture/refund = %d/%d/%d, want 1/0/0",
					h.gw.callCount("void"), h.gw.callCount("capture"), h.gw.callCount("refund"))
			}
			// A second settle (redelivery) is a no-op, not a second void.
			if _, err := h.uc.SettleDepositOnCancel(ctx, systemActor, p.BookingID, DepositCancelInput{Trigger: trig, CancelledAt: &now}); err != nil {
				t.Fatalf("second settle: %v", err)
			}
			if h.gw.callCount("void") != 1 {
				t.Fatalf("void called %d times after redelivery, want 1", h.gw.callCount("void"))
			}
		})
	}
}

// CaptureOnConfirm from cabinet + Telegram at once: exactly one /payments/confirm
// and one batch of ledger entries (spec criterion 4).
func TestCaptureOnConfirm_ConcurrentCallersCaptureOnce(t *testing.T) {
	ctx := context.Background()
	p := heldPreorder(uuid.New())
	h := newHoldHarness(t, p, true)
	c := h.uc.(PreorderConfirmationUseCase)

	var wg, start sync.WaitGroup
	start.Add(1)
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = c.CaptureOnConfirm(ctx, p.BookingID)
		}(i)
	}
	start.Done()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := h.gw.callCount("capture"); n != 1 {
		t.Fatalf("capture called %d times, want exactly 1", n)
	}
	stored, _ := h.repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", stored.Status)
	}
	if bal, _ := h.ledger.BalanceByAccount(ctx, p.ID); bal[domain.AccountRestaurant] != -p.BaseAmountMinor {
		t.Fatalf("restaurant credited %d, want one credit of %d", bal[domain.AccountRestaurant], p.BaseAmountMinor)
	}
}

// A one-stage pre-order (issued before the rollout) and a deposit have nothing to
// capture on confirmation.
func TestCaptureOnConfirm_NoHoldIsANoOp(t *testing.T) {
	ctx := context.Background()
	p := heldPreorder(uuid.New())
	p.RequiresConfirmation = false
	p.Status = domain.PaymentCaptured
	h := newHoldHarness(t, p, true)
	got, err := h.uc.(PreorderConfirmationUseCase).CaptureOnConfirm(ctx, p.BookingID)
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
	}
	if h.gw.callCount("capture") != 0 {
		t.Fatalf("capture called for a one-stage payment")
	}
}

// A definitive acquirer refusal is surfaced as ErrProviderDeclined so the
// booking is cancelled as the system (criterion 9).
func TestCaptureOnConfirm_DeclineIsReported(t *testing.T) {
	ctx := context.Background()
	p := heldPreorder(uuid.New())
	h := newHoldHarness(t, p, true)
	h.gw.captureErr = domain.ErrProviderDeclined
	_, err := h.uc.(PreorderConfirmationUseCase).CaptureOnConfirm(ctx, p.BookingID)
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("error = %v, want ErrProviderDeclined", err)
	}
}

type fakeReleaser struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeReleaser) ReleaseForPayment(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return nil
}

// created -> authorized of a pre-order hold releases the booking to the venue
// once, and does NOT capture (a pending booking waits for the venue).
func TestWebhookAuthorized_PreorderHoldReleasesBookingAndDoesNotCapture(t *testing.T) {
	ctx := context.Background()
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-hold")
	p.Purpose = domain.PurposePreorder
	u, repo, _, ledger, _, gw := newWebhookHarness(p)
	rel := &fakeReleaser{}
	u.releaser = rel
	u.bookings = newFakeBookingReader(&domain.Booking{ID: p.BookingID, Status: domain.BookingPending})
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-h", ProviderPaymentID: "gw-hold",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	for i := 0; i < 2; i++ { // second = redelivery of the same event
		if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentAuthorized || stored.CapturedAt != nil {
		t.Fatalf("status = %s captured_at=%v, want authorized and not captured", stored.Status, stored.CapturedAt)
	}
	if gw.callCount("capture") != 0 {
		t.Fatalf("capture called on a pending booking's hold")
	}
	if len(rel.calls) != 1 || rel.calls[0] != p.BookingID {
		t.Fatalf("release calls = %v, want exactly one for the booking", rel.calls)
	}
	if bal, _ := ledger.BalanceByAccount(ctx, p.ID); bal[domain.AccountRestaurant] != 0 {
		t.Fatalf("ledger booked %d for an uncaptured hold", bal[domain.AccountRestaurant])
	}
}

// An already confirmed booking (confirm_on_create / old client) is captured
// right after the hold lands.
func TestWebhookAuthorized_PreorderHoldOnConfirmedBookingCaptures(t *testing.T) {
	ctx := context.Background()
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-hold")
	p.Purpose = domain.PurposePreorder
	u, repo, _, _, _, gw := newWebhookHarness(p)
	u.bookings = newFakeBookingReader(&domain.Booking{ID: p.BookingID, Status: domain.BookingConfirmed})
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-c", ProviderPaymentID: "gw-hold",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if stored, _ := repo.GetByID(ctx, p.ID); stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", stored.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want 1", gw.callCount("capture"))
	}
}
