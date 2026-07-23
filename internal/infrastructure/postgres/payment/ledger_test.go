package payment

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestLedgerCreateBatchBalancedAndListed(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	ledger := NewLedger(pool)

	p := newCapturedPayment(t, ctx, payments, bid, rid)

	now := time.Now()
	entries := []domain.PaymentLedgerEntry{
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountGuest, Direction: domain.DirectionDebit, AmountMinor: p.AmountMinor, Currency: p.Currency, EntryType: domain.EntryCapture, CreatedAt: now},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountRestaurant, Direction: domain.DirectionCredit, AmountMinor: p.BaseAmountMinor, Currency: p.Currency, EntryType: domain.EntryCapture, CreatedAt: now},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountPlatform, Direction: domain.DirectionCredit, AmountMinor: p.FeeMinor, Currency: p.Currency, EntryType: domain.EntryServiceFee, CreatedAt: now},
	}
	if err := ledger.CreateBatch(ctx, entries); err != nil {
		t.Fatalf("create batch: %v", err)
	}

	list, err := ledger.ListByPaymentID(ctx, p.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("list by payment id: %d rows, err=%v", len(list), err)
	}

	balances, err := ledger.BalanceByAccount(ctx, p.ID)
	if err != nil {
		t.Fatalf("balance by account: %v", err)
	}
	// Debit = credit sides of the invariant: the guest's debit equals the
	// restaurant + platform credits combined.
	if balances[domain.AccountGuest] != p.AmountMinor {
		t.Errorf("guest balance = %d, want %d", balances[domain.AccountGuest], p.AmountMinor)
	}
	if balances[domain.AccountRestaurant] != -p.BaseAmountMinor {
		t.Errorf("restaurant balance = %d, want %d", balances[domain.AccountRestaurant], -p.BaseAmountMinor)
	}
	if balances[domain.AccountPlatform] != -p.FeeMinor {
		t.Errorf("platform balance = %d, want %d", balances[domain.AccountPlatform], -p.FeeMinor)
	}
}

// TestLedgerCreateBatchRejectsUnbalanced proves the repository defends the
// double-entry invariant itself (not only the callers that are supposed to
// check it first): an unbalanced batch is rejected before it ever reaches
// Postgres, and nothing is written.
func TestLedgerCreateBatchRejectsUnbalanced(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	ledger := NewLedger(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	unbalanced := []domain.PaymentLedgerEntry{
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountGuest, Direction: domain.DirectionDebit, AmountMinor: 100, Currency: p.Currency, EntryType: domain.EntryCapture},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountRestaurant, Direction: domain.DirectionCredit, AmountMinor: 50, Currency: p.Currency, EntryType: domain.EntryCapture},
	}
	if err := ledger.CreateBatch(ctx, unbalanced); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("create unbalanced batch = %v, want ErrValidation", err)
	}
	list, err := ledger.ListByPaymentID(ctx, p.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("unbalanced batch left %d rows behind, want 0 (err=%v)", len(list), err)
	}
}

// TestLedgerCreateBatchAtomicOnFKViolation proves the "batch is atomic" claim
// against a real constraint failure, not just the Go-side validation above: a
// batch where one entry references a refund_id that does not exist violates
// the FK inside the SAME multi-row INSERT the other, otherwise-valid entries
// are part of — Postgres must reject the whole statement, leaving nothing.
func TestLedgerCreateBatchAtomicOnFKViolation(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	ledger := NewLedger(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	badRefund := uuid.New() // never inserted into payment_refunds
	now := time.Now()
	entries := []domain.PaymentLedgerEntry{
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountGuest, Direction: domain.DirectionCredit, AmountMinor: 1000, Currency: p.Currency, EntryType: domain.EntryRefund, RefundID: &badRefund, CreatedAt: now},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountAcquirer, Direction: domain.DirectionDebit, AmountMinor: 1000, Currency: p.Currency, EntryType: domain.EntryAcquiring, CreatedAt: now},
	}
	if err := ledger.CreateBatch(ctx, entries); err == nil {
		t.Fatal("create batch with a bad refund_id succeeded, want a constraint failure")
	}
	list, err := ledger.ListByPaymentID(ctx, p.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("FK-violating batch left %d rows behind, want 0 (err=%v)", len(list), err)
	}
}
