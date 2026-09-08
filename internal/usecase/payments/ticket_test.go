package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ticketPaymentHarness wires the ticket-payment usecase over the shared fakes.
func ticketPaymentHarness(t *testing.T, feeBps int) (*ticketPaymentUseCase, *fakePaymentRepo, *fakeRefundRepo, *fakeLedgerRepo, *fakeGateway) {
	t.Helper()
	payments := newFakePaymentRepo()
	refunds := newFakeRefundRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	settings := newFakeRestaurantSettings()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	tx := &fakeTx{payments: payments, ledger: ledger, outbox: outbox, refunds: refunds}
	uc := NewTicketPaymentUseCase(payments, refunds, ledger, outbox, settings, resolver, managers, tx,
		Config{Enabled: true, ServiceFeeBps: feeBps}).(*ticketPaymentUseCase)
	return uc, payments, refunds, ledger, gw
}

// TestCreateForTicketGrossesUp proves a ticket purchase authorizes the
// grossed-up total (venue's take + acquirer fee) and stores the split, so the
// venue nets the full ticket price.
func TestCreateForTicketGrossesUp(t *testing.T) {
	uc, _, _, _, gw := ticketPaymentHarness(t, 350) // 3.5%
	ticketID := uuid.New()
	base := int64(70000) // 2 tickets × 35000

	p, err := uc.CreateForTicket(context.Background(), Actor{}, TicketPaymentInput{
		EventTicketID:   ticketID,
		RestaurantID:    uuid.New(),
		BaseAmountMinor: base,
		Currency:        domain.CurrencyKZT,
		IdempotencyKey:  "buy-1",
	})
	if err != nil {
		t.Fatalf("CreateForTicket: %v", err)
	}
	if p.Purpose != domain.PurposeTicket {
		t.Fatalf("purpose = %s, want ticket", p.Purpose)
	}
	if p.EventTicketID == nil || *p.EventTicketID != ticketID {
		t.Fatalf("event ticket id not linked: %v", p.EventTicketID)
	}
	if p.BaseAmountMinor != base {
		t.Fatalf("base = %d, want %d", p.BaseAmountMinor, base)
	}
	// gross-up: total = ceil(base / (1 - fee)); fee = total - base > 0.
	if p.AmountMinor <= base {
		t.Fatalf("total %d must exceed base %d (fee not added)", p.AmountMinor, base)
	}
	if p.AmountMinor != p.BaseAmountMinor+p.FeeMinor {
		t.Fatalf("amount split broken: %d != %d + %d", p.AmountMinor, p.BaseAmountMinor, p.FeeMinor)
	}
	if gw.callCount("authorize") != 1 {
		t.Fatalf("authorize called %d times, want 1", gw.callCount("authorize"))
	}
}

// TestCreateForTicketIdempotent proves a retry with the same key replays the
// same payment instead of authorizing twice.
func TestCreateForTicketIdempotent(t *testing.T) {
	uc, _, _, _, gw := ticketPaymentHarness(t, 350)
	ticketID := uuid.New()
	in := TicketPaymentInput{EventTicketID: ticketID, RestaurantID: uuid.New(), BaseAmountMinor: 35000, Currency: domain.CurrencyKZT, IdempotencyKey: "same"}

	p1, err := uc.CreateForTicket(context.Background(), Actor{}, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	p2, err := uc.CreateForTicket(context.Background(), Actor{}, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("retry created a new payment: %s vs %s", p1.ID, p2.ID)
	}
	if gw.callCount("authorize") != 1 {
		t.Fatalf("authorize called %d times on a retry, want 1", gw.callCount("authorize"))
	}
}

// TestRefundTicketFullMakeWhole proves a captured ticket payment refunds the
// full amount to the guest and reaches the terminal `refunded` status.
func TestRefundTicketFullMakeWhole(t *testing.T) {
	uc, payments, refunds, ledger, gw := ticketPaymentHarness(t, 350)
	ticketID := uuid.New()
	pid := "gw-abc"
	captured := &domain.Payment{
		ID: uuid.New(), EventTicketID: &ticketID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: &pid, Purpose: domain.PurposeTicket,
		Status: domain.PaymentCaptured, AmountMinor: 36269, BaseAmountMinor: 35000, FeeMinor: 1269,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
	}
	payments.byID[captured.ID] = captured

	p, err := uc.RefundTicket(context.Background(), Actor{}, TicketRefundInput{
		PaymentID: captured.ID, IdempotencyKey: "refund-1",
	})
	if err != nil {
		t.Fatalf("RefundTicket: %v", err)
	}
	if p.Status != domain.PaymentRefunded {
		t.Fatalf("status = %s, want refunded", p.Status)
	}
	if gw.callCount("refund") != 1 {
		t.Fatalf("gateway refund called %d times, want 1", gw.callCount("refund"))
	}
	// Exactly one succeeded refund of the full amount recorded.
	total, _ := refunds.SucceededTotal(context.Background(), captured.ID)
	if total != captured.AmountMinor {
		t.Fatalf("refunded total = %d, want %d", total, captured.AmountMinor)
	}
	// Ledger reversal is balanced (ValidateLedgerBalance ran in the usecase).
	if entries, _ := ledger.ListByPaymentID(context.Background(), captured.ID); len(entries) == 0 {
		t.Fatalf("no ledger reversal written")
	}

	// Idempotent retry: same key, no second acquirer refund.
	if _, err := uc.RefundTicket(context.Background(), Actor{}, TicketRefundInput{PaymentID: captured.ID, IdempotencyKey: "refund-1"}); err != nil {
		t.Fatalf("retry refund: %v", err)
	}
	if gw.callCount("refund") != 1 {
		t.Fatalf("retry caused a second gateway refund: %d", gw.callCount("refund"))
	}
}

// TestRefundTicketRejectsNonCaptured proves an uncaptured ticket payment cannot
// be refunded.
func TestRefundTicketRejectsNonCaptured(t *testing.T) {
	uc, payments, _, _, _ := ticketPaymentHarness(t, 350)
	ticketID := uuid.New()
	authorized := &domain.Payment{
		ID: uuid.New(), EventTicketID: &ticketID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, Purpose: domain.PurposeTicket,
		Status: domain.PaymentAuthorized, AmountMinor: 100, BaseAmountMinor: 100, Currency: domain.CurrencyKZT,
	}
	payments.byID[authorized.ID] = authorized
	_, err := uc.RefundTicket(context.Background(), Actor{}, TicketRefundInput{PaymentID: authorized.ID, IdempotencyKey: "r"})
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus", err)
	}
}
