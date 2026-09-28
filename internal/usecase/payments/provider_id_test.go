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

// find returns the LAST operation for the invoice, possibly a declined attempt:
// its id must not be persisted, and the real webhook id must win afterwards.
func TestEnsureTransactionID_DeclinedLastOperationThenWebhookWins(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCreated)
	u, repo, _, _, _, gw := newWebhookHarness(p)
	gw.placeholder = numericOnly
	gw.oneStage = func(domain.PaymentPurpose) bool { return true }
	gw.findFn = func(string) (*domain.GatewayPayment, error) {
		return &domain.GatewayPayment{ProviderPaymentID: "111", Status: domain.PaymentFailed}, nil
	}
	ctx := context.Background()
	cur, _ := repo.GetByID(ctx, p.ID)
	if err := ensureTransactionID(ctx, repo, gw, cur); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("ensureTransactionID() error = %v, want ErrInvalidStatus for a declined operation", err)
	}
	if got, _ := repo.GetByID(ctx, p.ID); *got.ProviderPaymentID != "ApWFpiKnfDuxMXSa" {
		t.Fatalf("declined id persisted: %q", *got.ProviderPaymentID)
	}

	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-ok", ProviderPaymentID: "222",
		MerchantPaymentID: p.ID.String(), Type: domain.WebhookPaymentCaptured, Status: domain.PaymentCaptured,
		Amount: domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}, SignatureValid: true,
	})
	if err := u.HandleWebhook(ctx, domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	if got, _ := repo.GetByID(ctx, p.ID); *got.ProviderPaymentID != "222" {
		t.Fatalf("final id = %q, want 222", *got.ProviderPaymentID)
	}
}

// A stale in-memory copy (order id) must not overwrite an id a concurrent
// webhook already stored: the compare-and-swap loses and the stored id is adopted.
func TestEnsureTransactionID_ConcurrentWebhookNotOverwritten(t *testing.T) {
	p := orderIDPayment(uuid.New(), domain.PaymentCaptured)
	repo := newFakePaymentRepo(p)
	gw := newFakeGateway(domain.ProviderFreedomPay)
	gw.placeholder = numericOnly
	ctx := context.Background()
	stale, _ := repo.GetByID(ctx, p.ID)
	gw.findFn = func(string) (*domain.GatewayPayment, error) {
		// the webhook lands while find is in flight
		_ = repo.SetProviderPaymentID(ctx, p.ID, stale.ProviderPaymentID, "222")
		return &domain.GatewayPayment{ProviderPaymentID: "333", Status: domain.PaymentCaptured}, nil
	}
	if err := ensureTransactionID(ctx, repo, gw, stale); err != nil {
		t.Fatalf("ensureTransactionID() error = %v", err)
	}
	if got, _ := repo.GetByID(ctx, p.ID); *got.ProviderPaymentID != "222" || *stale.ProviderPaymentID != "222" {
		t.Fatalf("stored=%q in-memory=%q, want both 222 (webhook wins)", *got.ProviderPaymentID, *stale.ProviderPaymentID)
	}
}

// A permanent id conflict (unique index: another payment owns the id) must not
// make the callback retry forever: adoptTransactionID logs and returns nil so the
// status transition still applies; the placeholder stays for ensureTransactionID.
func TestAdoptTransactionID_PermanentConflictDoesNotFailCallback(t *testing.T) {
	other := orderIDPayment(uuid.New(), domain.PaymentCaptured)
	other.ProviderPaymentID = strPtrTest("2726227")
	p := orderIDPayment(uuid.New(), domain.PaymentCreated)
	repo := newFakePaymentRepo(p, other)
	gw := newFakeGateway(domain.ProviderFreedomPay)
	gw.placeholder = numericOnly
	cur, _ := repo.GetByID(context.Background(), p.ID)
	err := adoptTransactionID(context.Background(), repo, gw, cur, &domain.WebhookEvent{
		Type: domain.WebhookPaymentCaptured, ProviderPaymentID: "2726227",
	})
	if err != nil {
		t.Fatalf("adoptTransactionID() error = %v, want nil (log and proceed)", err)
	}
	if got, _ := repo.GetByID(context.Background(), p.ID); *got.ProviderPaymentID != "ApWFpiKnfDuxMXSa" {
		t.Fatalf("id = %q, want the placeholder untouched", *got.ProviderPaymentID)
	}
}

// Ticket refund: a failing find (timeout) must leave the attempt retryable
// (created, not in_flight), send no refund, and a retry then succeeds once.
func TestRefundTicket_FindTimeoutLeavesAttemptRetryable(t *testing.T) {
	uc, payments, refunds, _, gw := ticketPaymentHarness(t, 350)
	gw.placeholder = numericOnly
	calls := 0
	gw.findFn = func(string) (*domain.GatewayPayment, error) {
		calls++
		if calls == 1 {
			return nil, domain.ErrProviderOutcomeUnknown
		}
		return &domain.GatewayPayment{ProviderPaymentID: "2726227", Status: domain.PaymentCaptured}, nil
	}
	ticketID := uuid.New()
	pid := "ApWFpiKnfDuxMXSa"
	captured := &domain.Payment{
		ID: uuid.New(), EventTicketID: &ticketID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: &pid, Purpose: domain.PurposeTicket,
		Status: domain.PaymentCaptured, AmountMinor: 36269, BaseAmountMinor: 35000, FeeMinor: 1269,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
	}
	payments.byID[captured.ID] = captured
	in := TicketRefundInput{PaymentID: captured.ID, IdempotencyKey: "refund-1"}
	ctx := context.Background()

	if _, err := uc.RefundTicket(ctx, Actor{}, in); err == nil {
		t.Fatalf("first RefundTicket() error = nil, want the find failure")
	}
	if gw.callCount("refund") != 0 {
		t.Fatalf("refund sent %d times after a failed lookup, want 0", gw.callCount("refund"))
	}
	rf, err := refunds.GetByIdempotencyKey(ctx, captured.ID, "refund-1")
	if err != nil || rf.Status != domain.RefundCreated {
		t.Fatalf("attempt = %+v err=%v, want status created (retryable)", rf, err)
	}
	if _, err := uc.RefundTicket(ctx, Actor{}, in); err != nil {
		t.Fatalf("retry RefundTicket() error = %v", err)
	}
	if gw.callCount("refund") != 1 || gw.refundIDs[0] != "2726227" {
		t.Fatalf("refunds=%d ids=%v, want exactly one with 2726227", gw.callCount("refund"), gw.refundIDs)
	}
}
