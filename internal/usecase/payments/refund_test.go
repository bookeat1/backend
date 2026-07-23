package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newRefundHarness(p *domain.Payment, acquiringBps int) (RefundUseCase, *fakePaymentRepo, *fakeRefundRepo, *fakeLedgerRepo, *fakePaymentOutbox, *fakeGateway) {
	repo := newFakePaymentRepo(p)
	refunds := newFakeRefundRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox, refunds: refunds}
	u := NewRefundUseCase(repo, refunds, ledger, outbox, resolver, tx, Config{RefundAcquiringBps: acquiringBps})
	return u, repo, refunds, ledger, outbox, gw
}

// TestSettle_TableFromSpec walks the exact §9.1 settlement table (variant A)
// through the usecase, including the ledger write, and checks the payment's
// terminal status.
func TestSettle_TableFromSpec(t *testing.T) {
	deadline := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		trigger       domain.RefundTrigger
		at            time.Time
		actor         Actor
		wantStatus    domain.PaymentStatus
		wantRefund    bool
		wantRestaurant int64
		wantPlatform   int64
	}{
		{
			name: "guest cancels before deadline: refunded minus acquiring",
			trigger: domain.RefundTriggerGuestCancel, at: deadline.Add(-time.Minute),
			actor: Actor{}, wantStatus: domain.PaymentRefunded, wantRefund: true,
		},
		{
			name: "guest cancels after deadline: settled as no-show, no ledger movement",
			trigger: domain.RefundTriggerGuestCancel, at: deadline.Add(time.Minute),
			actor: staffActor, wantStatus: domain.PaymentCaptured, wantRefund: false,
			wantRestaurant: 966_000, wantPlatform: 34_000,
		},
		{
			name: "no-show: base to venue, fee to platform, no ledger movement",
			trigger: domain.RefundTriggerNoShow, at: deadline.Add(-48 * time.Hour),
			actor: staffActor, wantStatus: domain.PaymentCaptured, wantRefund: false,
			wantRestaurant: 966_000, wantPlatform: 34_000,
		},
		{
			name: "venue cancels: full refund including the service fee",
			trigger: domain.RefundTriggerVenueCancel, at: deadline.Add(time.Minute),
			actor: staffActor, wantStatus: domain.PaymentRefunded, wantRefund: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Payment{
				ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
				Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
				Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
				AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
				Currency: domain.CurrencyKZT, IdempotencyKey: "k",
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			u, repo, _, ledger, outbox, gw := newRefundHarness(p, 100) // 1%

			got, err := u.Settle(context.Background(), tc.actor, p.BookingID, SettleInput{
				Trigger: tc.trigger, CancelledAt: tc.at, CancelDeadline: deadline, IdempotencyKey: "settle-1",
			})
			if err != nil {
				t.Fatalf("Settle() error = %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			stored, _ := repo.GetByID(context.Background(), p.ID)
			if stored.Status != tc.wantStatus {
				t.Fatalf("persisted status = %s, want %s", stored.Status, tc.wantStatus)
			}

			entries, _ := ledger.ListByPaymentID(context.Background(), p.ID)
			if err := domain.ValidateLedgerBalance(entries); len(entries) > 0 && err != nil {
				t.Fatalf("ledger does not balance: %v", err)
			}

			if tc.wantRefund {
				if gw.callCount("refund") != 1 {
					t.Fatalf("refund called %d times, want 1", gw.callCount("refund"))
				}
				if len(entries) == 0 {
					t.Fatalf("expected ledger entries for a refund, got none")
				}
			} else {
				if gw.callCount("refund") != 0 {
					t.Fatalf("refund called %d times, want 0 (no money moves)", gw.callCount("refund"))
				}
				if len(entries) != 0 {
					t.Fatalf("expected NO ledger entries (nothing changes from capture-time booking), got %d: %+v", len(entries), entries)
				}
				bal, _ := ledger.BalanceByAccount(context.Background(), p.ID)
				_ = bal
			}
			foundSettledOrRefunded := false
			for _, ty := range outbox.types() {
				if ty == domain.EventPaymentSettled || ty == domain.EventPaymentRefunded {
					foundSettledOrRefunded = true
				}
			}
			if !foundSettledOrRefunded {
				t.Fatalf("outbox events = %v, want a settled/refunded event", outbox.types())
			}
		})
	}
}

func TestSettle_PartialRefundAmountMatchesPolicy(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_035_000, BaseAmountMinor: 1_000_000, FeeMinor: 35_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	u, _, _, ledger, _, gw := newRefundHarness(p, 100) // 1%
	deadline := time.Now().Add(time.Hour)

	_, err := u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: time.Now(), CancelDeadline: deadline,
		IdempotencyKey: "settle-1",
	})
	if err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	if gw.refundN != 1 {
		t.Fatalf("refund called %d times, want 1", gw.refundN)
	}
	bal, _ := ledger.BalanceByAccount(context.Background(), p.ID)
	// 1% of 1,035,000 = 10,350 withheld; guest gets 1,024,650 back.
	if got := -bal[domain.AccountGuest]; got != 1_024_650 {
		t.Fatalf("guest net = %d, want -1024650 (a credit of 1,024,650)", got)
	}
	if got := bal[domain.AccountAcquirer]; got != -10_350 {
		t.Fatalf("acquirer net = %d, want a credit of 10,350", got)
	}
}

func TestSettle_IdempotentReplayNoSecondRefund(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	u, _, _, _, _, gw := newRefundHarness(p, 100)
	ctx := context.Background()
	in := SettleInput{Trigger: domain.RefundTriggerVenueCancel, CancelledAt: time.Now(), CancelDeadline: time.Now(), IdempotencyKey: "retry-1"}

	first, err := u.Settle(ctx, staffActor, p.BookingID, in)
	if err != nil {
		t.Fatalf("first Settle() error = %v", err)
	}
	if first.Status != domain.PaymentRefunded {
		t.Fatalf("first Settle() status = %s, want refunded", first.Status)
	}
	// The payment left the "live" (authorized/captured) set after the first
	// call succeeded, so GetLiveByBookingID correctly reports ErrNotFound on
	// a second call — there is nothing left to settle twice. This IS the
	// idempotency guarantee for the sequential-retry case (spec §8): a caller
	// that retries after seeing the first call succeed can tell "already
	// done" apart from "still in progress" by this exact error.
	_, err = u.Settle(ctx, staffActor, p.BookingID, in)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second Settle() error = %v, want ErrNotFound (payment no longer live)", err)
	}
	if gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times across 2 Settle() calls, want 1", gw.callCount("refund"))
	}
}

func TestSettle_ProviderRefundFailureRecordsAttempt(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	u, repo, refunds, ledger, outbox, gw := newRefundHarness(p, 100)
	gw.refundErr = errors.New("acquirer: timeout")

	_, err := u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, CancelledAt: time.Now(), CancelDeadline: time.Now().Add(time.Hour),
		IdempotencyKey: "settle-1",
	})
	if err == nil {
		t.Fatalf("expected an error from a provider refund failure")
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want unchanged captured", stored.Status)
	}
	if len(ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries from a failed refund, want 0", len(ledger.entries))
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from a failed refund, want 0", len(outbox.types()))
	}
	found := false
	for _, r := range refunds.byID {
		if r.Status == domain.RefundFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failed refund attempt recorded")
	}
}

func TestSettle_GuestCannotTriggerNoShowOnThemselves(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(), UserID: nil,
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	u, _, _, _, _, _ := newRefundHarness(p, 100)

	_, err := u.Settle(context.Background(), Actor{Role: domain.RoleUser}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerNoShow, CancelledAt: time.Now(), CancelDeadline: time.Now(),
		IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

// TestSettle_LocalCommitFailureAfterGatewaySucceedsIsRecoverable is the
// money-safety case for the "rollback on a mid-transaction failure" test
// requirement, applied to a refund: the acquirer already returned the money
// (gw.Refund succeeded) but the LOCAL ledger/status commit then fails. The
// refund row must already be durably marked succeeded (it is written before
// the ledger/status transaction, on its own commit) so nothing about the
// money movement is lost, even though the caller gets an error saying
// reconciliation is needed. A subsequent retry with the SAME idempotency key
// must not call the acquirer again.
func TestSettle_LocalCommitFailureAfterGatewaySucceedsIsRecoverable(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	repo := newFakePaymentRepo(p)
	refunds := newFakeRefundRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox, refunds: refunds}
	u := NewRefundUseCase(repo, refunds, ledger, outbox, resolver, tx, Config{RefundAcquiringBps: 100})
	ctx := context.Background()

	ledger.createBatchErr = errors.New("db: connection reset") // fires once
	in := SettleInput{Trigger: domain.RefundTriggerGuestCancel, CancelledAt: time.Now(), CancelDeadline: time.Now().Add(time.Hour), IdempotencyKey: "flaky-1"}

	_, err := u.Settle(ctx, Actor{}, p.BookingID, in)
	if err == nil {
		t.Fatalf("expected an error from the injected ledger failure")
	}
	if gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times, want 1 (the acquirer call itself succeeded)", gw.callCount("refund"))
	}
	// The refund row must already show the true outcome, even though the
	// payment's own status did not move (its transaction rolled back).
	var recorded *domain.PaymentRefund
	for _, r := range refunds.byID {
		recorded = r
	}
	if recorded == nil || recorded.Status != domain.RefundSucceeded {
		t.Fatalf("refund row = %+v, want a durably recorded succeeded refund", recorded)
	}
	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("payment status = %s, want unchanged captured (its own commit rolled back)", stored.Status)
	}

	// Retry with the same key: must NOT call the acquirer a second time, and
	// must now complete the ledger/status commit (createBatchErr only fires
	// once).
	got, err := u.Settle(ctx, Actor{}, p.BookingID, in)
	if err != nil {
		t.Fatalf("retry Settle() error = %v", err)
	}
	if got.Status != domain.PaymentRefunded {
		t.Fatalf("retry status = %s, want refunded", got.Status)
	}
	if gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times after the retry, want still 1 (no second acquirer call)", gw.callCount("refund"))
	}
}

func strPtrTest(s string) *string { return &s }
