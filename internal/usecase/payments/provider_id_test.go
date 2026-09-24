package payments

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// numericOnly mimics tiptoppay.Gateway.IsPlaceholderProviderID: anything that
// is not a numeric TransactionId is the ORDER id Authorize returned.
func numericOnly(id string) bool {
	_, err := strconv.ParseInt(id, 10, 64)
	return err != nil
}

func orderIDPayment(bookingID uuid.UUID, status domain.PaymentStatus) *domain.Payment {
	p := capturedTestPayment(bookingID)
	p.Provider = domain.ProviderFreedomPay // the harness gateway registers under this name
	p.ProviderPaymentID = strPtrTest("ApWFpiKnfDuxMXSa")
	p.Status = status
	p.Purpose = domain.PurposePreorder
	return p
}

// The webhook carries the numeric TransactionId; the payment row still holds
// the order id. After the callback the row must hold the transaction id.
func TestHandleWebhook_CapturedAdoptsTransactionID(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCreated)
	u, repo, _, _, _, gw := newWebhookHarness(p)
	gw.placeholder = numericOnly
	gw.oneStage = func(domain.PaymentPurpose) bool { return true }
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-1", ProviderPaymentID: "2726227",
		MerchantPaymentID: p.ID.String(),
		Type:              domain.WebhookPaymentCaptured, Status: domain.PaymentCaptured,
		Amount: domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}, SignatureValid: true,
	})

	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	got, _ := repo.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	if got.ProviderPaymentID == nil || *got.ProviderPaymentID != "2726227" {
		t.Fatalf("provider_payment_id = %v, want the transaction id 2726227", got.ProviderPaymentID)
	}
}

// A declined attempt has its own TransactionId; it must never replace the id
// of a payment that already went through.
func TestHandleWebhook_FailedDoesNotOverwriteProviderID(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCreated)
	u, repo, _, _, _, gw := newWebhookHarness(p)
	gw.placeholder = numericOnly
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-f", ProviderPaymentID: "111",
		MerchantPaymentID: p.ID.String(),
		Type:              domain.WebhookPaymentFailed, Status: domain.PaymentFailed, SignatureValid: true,
	})
	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	got, _ := repo.GetByID(context.Background(), p.ID)
	if *got.ProviderPaymentID != "ApWFpiKnfDuxMXSa" {
		t.Fatalf("provider_payment_id = %q, a failed attempt must not overwrite it", *got.ProviderPaymentID)
	}
}

// An acquirer without the placeholder capability keeps today's behaviour: the
// id it stored is authoritative and is never rewritten from a callback.
func TestHandleWebhook_NoPlaceholderCapabilityLeavesIDAlone(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCreated)
	u, repo, _, _, _, gw := newWebhookHarness(p)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-a", ProviderPaymentID: "999",
		MerchantPaymentID: p.ID.String(),
		Type:              domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	got, _ := repo.GetByID(context.Background(), p.ID)
	if *got.ProviderPaymentID != "ApWFpiKnfDuxMXSa" {
		t.Fatalf("provider_payment_id = %q, want unchanged", *got.ProviderPaymentID)
	}
}

// The reported bug: a payment captured before the id was stored still holds the
// order id. Refund must resolve the transaction id via find-by-order first and
// send THAT to the acquirer.
func TestSettle_RefundResolvesTransactionIDForOrderIDPayment(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCaptured)
	h := newRefundHarness(p, 100)
	h.gw.placeholder = numericOnly
	h.gw.findFn = func(merchantID string) (*domain.GatewayPayment, error) {
		if merchantID != p.ID.String() {
			t.Errorf("find called with %q, want our payment id %q", merchantID, p.ID)
		}
		return &domain.GatewayPayment{ProviderPaymentID: "2726227", Status: domain.PaymentCaptured}, nil
	}
	h.setCancelledAt(p.BookingID, time.Now())

	out, err := h.u.Settle(context.Background(), staffActor, p.BookingID,
		SettleInput{Trigger: domain.RefundTriggerVenueCancel, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	if out.Status != domain.PaymentRefunded {
		t.Fatalf("status = %s, want refunded", out.Status)
	}
	if len(h.gw.refundIDs) != 1 || h.gw.refundIDs[0] != "2726227" {
		t.Fatalf("refund got provider ids %v, want [2726227]", h.gw.refundIDs)
	}
	stored, _ := h.payments.GetByID(context.Background(), p.ID)
	if *stored.ProviderPaymentID != "2726227" {
		t.Fatalf("stored id = %q, want healed 2726227", *stored.ProviderPaymentID)
	}
}

// The guest never paid: the acquirer finds no transaction. The refund must not
// be sent and the attempt must stay `created` (retryable), not `pending`.
func TestSettle_RefundUnresolvableOrderIDDoesNotCallRefund(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCaptured)
	h := newRefundHarness(p, 100)
	h.gw.placeholder = numericOnly
	h.gw.findFn = func(string) (*domain.GatewayPayment, error) {
		return nil, errors.New("tiptoppay payments/find: Transaction not found")
	}
	h.setCancelledAt(p.BookingID, time.Now())

	_, err := h.u.Settle(context.Background(), staffActor, p.BookingID,
		SettleInput{Trigger: domain.RefundTriggerVenueCancel, IdempotencyKey: "k2"})
	if err == nil {
		t.Fatalf("Settle() error = nil, want the resolution failure")
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0", h.gw.callCount("refund"))
	}
	rf, gerr := h.refunds.GetByIdempotencyKey(context.Background(), p.ID, "k2")
	if gerr != nil {
		t.Fatalf("refund row missing: %v", gerr)
	}
	if rf.Status != domain.RefundCreated {
		t.Fatalf("refund status = %s, want created (retryable)", rf.Status)
	}
}

// A payment that already holds a numeric id must not cost an extra acquirer call.
func TestSettle_NumericIDSkipsLookup(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCaptured)
	p.ProviderPaymentID = strPtrTest("2726227")
	h := newRefundHarness(p, 100)
	h.gw.placeholder = numericOnly
	h.setCancelledAt(p.BookingID, time.Now())
	if _, err := h.u.Settle(context.Background(), staffActor, p.BookingID,
		SettleInput{Trigger: domain.RefundTriggerVenueCancel, IdempotencyKey: "k3"}); err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	if h.gw.findN != 0 {
		t.Fatalf("find called %d times, want 0", h.gw.findN)
	}
}
