package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newCapturedPayment(t *testing.T, ctx context.Context, repo *Repository, bookingID, restaurantID uuid.UUID) *domain.Payment {
	t.Helper()
	p := newPayment(bookingID, restaurantID)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create payment: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, time.Now()); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

func newRefund(paymentID uuid.UUID, amount int64, key string) *domain.PaymentRefund {
	return &domain.PaymentRefund{
		ID: uuid.New(), PaymentID: paymentID, AmountMinor: amount, Currency: domain.CurrencyKZT,
		Status: domain.RefundCreated, IdempotencyKey: key,
	}
}

func TestRefundCRUDAndSucceededTotal(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	refunds := NewRefunds(pool)

	p := newCapturedPayment(t, ctx, payments, bid, rid)

	rf := newRefund(p.ID, 100000, "refund-key-1")
	if err := refunds.Create(ctx, rf); err != nil {
		t.Fatalf("create refund: %v", err)
	}
	got, err := refunds.GetByID(ctx, rf.ID)
	if err != nil || got.Status != domain.RefundCreated || got.AmountMinor != 100000 {
		t.Fatalf("roundtrip mismatch: %+v, err=%v", got, err)
	}
	if got.StatusChangedAt.IsZero() {
		t.Error("status_changed_at not stamped on insert")
	}

	if _, err := refunds.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get(missing) = %v, want ErrNotFound", err)
	}

	byKey, err := refunds.GetByIdempotencyKey(ctx, p.ID, "refund-key-1")
	if err != nil || byKey.ID != rf.ID {
		t.Fatalf("get by idempotency key: %+v, err=%v", byKey, err)
	}
	if _, err := refunds.GetByIdempotencyKey(ctx, p.ID, "unused"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get by idempotency key(unused) = %v, want ErrNotFound", err)
	}

	total, err := refunds.SucceededTotal(ctx, p.ID)
	if err != nil || total != 0 {
		t.Fatalf("succeeded total (before success) = %d, err=%v, want 0", total, err)
	}

	if err := refunds.CompareAndSwapStatus(ctx, rf.ID, domain.RefundCreated, domain.RefundInFlight, time.Now()); err != nil {
		t.Fatalf("claim in_flight: %v", err)
	}
	rf.Status = domain.RefundInFlight
	providerRefundID := "pr-1"
	rf.ProviderRefundID = &providerRefundID
	rf.Status = domain.RefundSucceeded
	rf.UpdatedAt = time.Now()
	if err := refunds.Update(ctx, rf); err != nil {
		t.Fatalf("update: %v", err)
	}
	total, err = refunds.SucceededTotal(ctx, p.ID)
	if err != nil || total != 100000 {
		t.Fatalf("succeeded total (after success) = %d, err=%v, want 100000", total, err)
	}

	list, err := refunds.ListByPaymentID(ctx, p.ID)
	if err != nil || len(list) != 1 || list[0].ID != rf.ID {
		t.Fatalf("list by payment: %+v, err=%v", list, err)
	}
}

// TestRefundDuplicateIdempotencyKeyConflicts proves
// idx_payment_refunds_idempotency is translated into domain.ErrAlreadyExists.
func TestRefundDuplicateIdempotencyKeyConflicts(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	refunds := NewRefunds(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	rf1 := newRefund(p.ID, 10000, "dup-key")
	if err := refunds.Create(ctx, rf1); err != nil {
		t.Fatalf("create rf1: %v", err)
	}
	rf2 := newRefund(p.ID, 20000, "dup-key")
	if err := refunds.Create(ctx, rf2); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create rf2 (duplicate idempotency key) = %v, want ErrAlreadyExists", err)
	}
}

// TestRefundDuplicateProviderRefundIDConflicts proves
// idx_payment_refunds_provider is translated into domain.ErrAlreadyExists —
// two refund rows must never claim the same acquirer-side refund id.
func TestRefundDuplicateProviderRefundIDConflicts(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	refunds := NewRefunds(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	providerRefundID := "shared-provider-refund-id"
	rf1 := newRefund(p.ID, 10000, "key-1")
	rf1.ProviderRefundID = &providerRefundID
	if err := refunds.Create(ctx, rf1); err != nil {
		t.Fatalf("create rf1: %v", err)
	}
	rf2 := newRefund(p.ID, 20000, "key-2")
	rf2.ProviderRefundID = &providerRefundID
	if err := refunds.Create(ctx, rf2); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create rf2 (duplicate provider refund id) = %v, want ErrAlreadyExists", err)
	}
}

func TestRefundCompareAndSwapStatus(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	refunds := NewRefunds(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	rf := newRefund(p.ID, 50000, "key")
	if err := refunds.Create(ctx, rf); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wrong expected status: rejected, no change.
	if err := refunds.CompareAndSwapStatus(ctx, rf.ID, domain.RefundSucceeded, domain.RefundFailed, time.Now()); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("CAS from wrong expected status = %v, want ErrAlreadyExists", err)
	}
	current, _ := refunds.GetByID(ctx, rf.ID)
	if current.Status != domain.RefundCreated {
		t.Fatalf("status changed despite a failed CAS: %s", current.Status)
	}

	if err := refunds.CompareAndSwapStatus(ctx, rf.ID, domain.RefundCreated, domain.RefundInFlight, time.Now()); err != nil {
		t.Fatalf("claim in_flight: %v", err)
	}
	current, _ = refunds.GetByID(ctx, rf.ID)
	if current.Status != domain.RefundInFlight {
		t.Fatalf("CAS did not apply: %+v", current)
	}

	if err := refunds.CompareAndSwapStatus(ctx, uuid.New(), domain.RefundCreated, domain.RefundInFlight, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("CAS(missing id) = %v, want ErrNotFound", err)
	}
}

// TestRefundCompareAndSwapStatusConcurrent is the refund-side twin of
// TestPaymentCompareAndSwapStatusConcurrent: two goroutines race the SAME
// created→in_flight claim (this is exactly what makes claimAndCallGateway's
// single-acquirer-call guarantee real). Exactly one must win.
func TestRefundCompareAndSwapStatusConcurrent(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	refunds := NewRefunds(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	rf := newRefund(p.ID, 50000, "race-key")
	if err := refunds.Create(ctx, rf); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	var startWG sync.WaitGroup
	startWG.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			startWG.Wait()
			errs[i] = refunds.CompareAndSwapStatus(context.Background(), rf.ID,
				domain.RefundCreated, domain.RefundInFlight, time.Now())
		}(i)
	}
	startWG.Done()
	wg.Wait()

	wins, losses := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, domain.ErrAlreadyExists):
			losses++
		default:
			t.Fatalf("unexpected error from concurrent CAS: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (losses=%d)", wins, losses)
	}
	if losses != n-1 {
		t.Fatalf("losses = %d, want %d", losses, n-1)
	}
}

func TestRefundClaimStaleAndRecordReconcileAttempt(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	payments := New(pool)
	refunds := NewRefunds(pool)

	mkStuck := func(agoHours int) *domain.PaymentRefund {
		bid := seedBooking(t, pool, rid)
		p := newCapturedPayment(t, ctx, payments, bid, rid)
		rf := newRefund(p.ID, 10000, uuid.New().String())
		if err := refunds.Create(ctx, rf); err != nil {
			t.Fatalf("create refund: %v", err)
		}
		claimedAt := time.Now().Add(-time.Duration(agoHours) * time.Hour)
		if err := refunds.CompareAndSwapStatus(ctx, rf.ID, domain.RefundCreated, domain.RefundInFlight, claimedAt); err != nil {
			t.Fatalf("claim in_flight: %v", err)
		}
		return rf
	}

	stale := mkStuck(2)
	fresh := mkStuck(0)

	cutoff := time.Now().Add(-time.Hour)
	due, err := refunds.ClaimStale(ctx, []domain.RefundStatus{domain.RefundInFlight, domain.RefundPending}, cutoff, 10)
	if err != nil {
		t.Fatalf("claim stale: %v", err)
	}
	if len(due) != 1 || due[0].ID != stale.ID {
		t.Fatalf("claim stale = %d rows, want only the stale one (fresh id=%s must not appear)", len(due), fresh.ID)
	}

	attempts, needsReview, err := refunds.RecordReconcileAttempt(ctx, stale.ID, domain.RefundInFlight, time.Now(), 5)
	if err != nil || attempts != 1 || needsReview {
		t.Fatalf("record attempt: attempts=%d needsReview=%v err=%v", attempts, needsReview, err)
	}

	if _, _, err := refunds.RecordReconcileAttempt(ctx, uuid.New(), domain.RefundInFlight, time.Now(), 5); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record attempt(missing) = %v, want ErrNotFound", err)
	}
	if _, _, err := refunds.RecordReconcileAttempt(ctx, stale.ID, domain.RefundSucceeded, time.Now(), 5); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("record attempt(wrong expected status) = %v, want ErrAlreadyExists", err)
	}
}
