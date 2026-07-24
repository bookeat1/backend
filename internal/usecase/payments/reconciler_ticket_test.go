package payments

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// recordingObserver captures the payment states the reconciler asks it to
// project onto a ticket, proving the reconciler-driven transitions reach the
// ticket layer (the projection itself is proven in usecase/tickets).
type recordingObserver struct {
	mu   sync.Mutex
	seen []struct {
		ticketID uuid.UUID
		status   domain.PaymentStatus
	}
}

func (o *recordingObserver) OnPaymentApplied(_ context.Context, p *domain.Payment) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, struct {
		ticketID uuid.UUID
		status   domain.PaymentStatus
	}{*p.EventTicketID, p.Status})
	return nil
}

func (o *recordingObserver) sawStatus(ticketID uuid.UUID, status domain.PaymentStatus) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.seen {
		if s.ticketID == ticketID && s.status == status {
			return true
		}
	}
	return false
}

func ticketPayment(ticketID uuid.UUID, status domain.PaymentStatus, providerID string) *domain.Payment {
	base := int64(70000)
	fee := int64(2538)
	pid := providerID
	return &domain.Payment{
		ID: uuid.New(), EventTicketID: &ticketID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: &pid, Purpose: domain.PurposeTicket,
		Status: status, AmountMinor: base + fee, BaseAmountMinor: base, FeeMinor: fee,
		Currency: domain.CurrencyKZT, IdempotencyKey: "ticket:" + ticketID.String(),
	}
}

// TestReconciler_ExpiredTicketHold_ProjectsCancel: the reconciler voids an
// expired ticket hold and the projection reaches the ticket (→ cancelled, seat
// freed) via the observer — previously nil, so the seat was stranded forever.
func TestReconciler_ExpiredTicketHold_ProjectsCancel(t *testing.T) {
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, LostWebhookAfter: time.Hour, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	rec := &recordingObserver{}
	h.r.ticketObserver = rec

	ticketID := uuid.New()
	p := ticketPayment(ticketID, domain.PaymentAuthorized, "gw-1")
	expired := h.now.Add(-time.Hour)
	p.ExpiresAt = &expired
	p.StatusChangedAt = h.now // recent, so only the expired-hold pass acts on it
	h.payments.byID[p.ID] = p
	// Acquirer confirms the hold is still just authorized → the worker voids it.
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentAuthorized}

	if _, err := h.r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentVoided {
		t.Fatalf("payment status = %s, want voided", got.Status)
	}
	if !rec.sawStatus(ticketID, domain.PaymentVoided) {
		t.Fatalf("observer was never asked to project the voided ticket payment: %+v", rec.seen)
	}
}

// TestReconciler_LostTicketCapture_ProjectsPaid: a lost capture webhook — the
// acquirer captured but we never heard — is synced in by the reconciler and
// projected onto the ticket (→ paid), so a paid guest is not left ticketless.
func TestReconciler_LostTicketCapture_ProjectsPaid(t *testing.T) {
	cfg := ReconcilerConfig{StuckAfter: 10 * time.Minute, LostWebhookAfter: time.Hour, BatchSize: 10, MaxAttempts: 3}
	h := newReconcilerHarness(t, cfg, nil, nil)
	rec := &recordingObserver{}
	h.r.ticketObserver = rec

	ticketID := uuid.New()
	p := ticketPayment(ticketID, domain.PaymentAuthorized, "gw-1")
	future := h.now.Add(time.Hour)
	p.ExpiresAt = &future                         // not expired → only the lost-webhook pass acts
	p.StatusChangedAt = h.now.Add(-2 * time.Hour) // stale past LostWebhookAfter
	h.payments.byID[p.ID] = p
	// Acquirer says it actually captured.
	h.gw.getResp = &domain.GatewayPayment{ProviderPaymentID: "gw-1", Status: domain.PaymentCaptured, Amount: p.Total()}

	if _, err := h.r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got, _ := h.payments.GetByID(context.Background(), p.ID)
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("payment status = %s, want captured", got.Status)
	}
	if !rec.sawStatus(ticketID, domain.PaymentCaptured) {
		t.Fatalf("observer was never asked to project the captured ticket payment: %+v", rec.seen)
	}
}
