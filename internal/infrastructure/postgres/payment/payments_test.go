package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

func TestPaymentCRUDAndList(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PaymentCreated || got.AmountMinor != p.AmountMinor ||
		got.BaseAmountMinor != p.BaseAmountMinor || got.FeeMinor != p.FeeMinor {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Currency != domain.CurrencyKZT || got.Provider != domain.ProviderFreedomPay {
		t.Fatalf("roundtrip currency/provider mismatch: %+v", got)
	}
	if got.StatusChangedAt.IsZero() {
		t.Error("status_changed_at not stamped on insert")
	}

	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get(missing) = %v, want ErrNotFound", err)
	}

	// GetByIdempotencyKey resolves our own retry token.
	byKey, err := repo.GetByIdempotencyKey(ctx, domain.ProviderFreedomPay, p.IdempotencyKey)
	if err != nil || byKey.ID != p.ID {
		t.Fatalf("get by idempotency key: %+v, err=%v", byKey, err)
	}
	if _, err := repo.GetByIdempotencyKey(ctx, domain.ProviderFreedomPay, "unused-key"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get by idempotency key(unused) = %v, want ErrNotFound", err)
	}

	// GetByProviderPaymentID: nil until the acquirer answers.
	if _, err := repo.GetByProviderPaymentID(ctx, domain.ProviderFreedomPay, "gw-123"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get by provider payment id(unset) = %v, want ErrNotFound", err)
	}
	pid := "gw-" + p.ID.String()
	p.ProviderPaymentID = &pid
	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	byProviderID, err := repo.GetByProviderPaymentID(ctx, domain.ProviderFreedomPay, pid)
	if err != nil || byProviderID.ID != p.ID {
		t.Fatalf("get by provider payment id: %+v, err=%v", byProviderID, err)
	}

	// GetLiveByBookingID: `created` is deliberately not live.
	if _, err := repo.GetLiveByBookingID(ctx, bid); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get live(created) = %v, want ErrNotFound", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	live, err := repo.GetLiveByBookingID(ctx, bid)
	if err != nil || live.ID != p.ID || live.Status != domain.PaymentAuthorized {
		t.Fatalf("get live(authorized): %+v, err=%v", live, err)
	}
	if live.AuthorizedAt == nil {
		t.Error("authorize did not stamp authorized_at")
	}

	// List by booking and by restaurant.
	items, total, err := repo.List(ctx, domain.PaymentFilter{BookingID: &bid})
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != p.ID {
		t.Fatalf("list by booking: rows=%d total=%d err=%v", len(items), total, err)
	}
	_, total, err = repo.List(ctx, domain.PaymentFilter{RestaurantID: &rid, Statuses: []domain.PaymentStatus{domain.PaymentCaptured}})
	if err != nil || total != 0 {
		t.Errorf("list(captured) total=%d err=%v, want 0", total, err)
	}
}

// TestPaymentCreateDuplicateIdempotencyKeyConflicts proves
// idx_payments_idempotency is translated into domain.ErrAlreadyExists, not a
// raw driver error — this is the guard that stops a lost-response retry from
// placing a second hold.
func TestPaymentCreateDuplicateIdempotencyKeyConflicts(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid1 := seedBooking(t, pool, rid)
	bid2 := seedBooking(t, pool, rid)
	repo := New(pool)

	key := "shared-key"
	p1 := newPayment(bid1, rid)
	p1.IdempotencyKey = key
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	p2 := newPayment(bid2, rid)
	p2.IdempotencyKey = key // same provider + key, different booking
	if err := repo.Create(ctx, p2); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create p2 (duplicate idempotency key) = %v, want ErrAlreadyExists", err)
	}

	// Nothing was silently duplicated: p2 never made it into the table.
	if _, err := repo.GetByID(ctx, p2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("p2 leaked into the table despite the conflict: err=%v", err)
	}
}

// TestGetSettleableByBookingID_FindsRefundedWhereLiveDoesNot is the
// regression test for the bug RefundUseCase.Settle used to hit: once a
// payment moves to `refunded`, idx_payments_live_per_booking (and therefore
// GetLiveByBookingID) no longer counts it as "live", but Settle still needs
// to find it — for a retried settle call to resume idempotently instead of
// 404ing. GetSettleableByBookingID is the wider lookup that fixes this.
func TestGetSettleableByBookingID_FindsRefundedWhereLiveDoesNot(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, time.Now()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Still captured: both lookups find it.
	if live, err := repo.GetLiveByBookingID(ctx, bid); err != nil || live.ID != p.ID {
		t.Fatalf("get live(captured): %+v, err=%v", live, err)
	}
	if settleable, err := repo.GetSettleableByBookingID(ctx, bid); err != nil || settleable.ID != p.ID {
		t.Fatalf("get settleable(captured): %+v, err=%v", settleable, err)
	}

	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, domain.PaymentRefunded, time.Now()); err != nil {
		t.Fatalf("refund: %v", err)
	}

	// Refunded: GetLiveByBookingID no longer finds it (the bug's root cause)...
	if _, err := repo.GetLiveByBookingID(ctx, bid); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get live(refunded) = %v, want ErrNotFound", err)
	}
	// ...but GetSettleableByBookingID still does (the fix).
	settleable, err := repo.GetSettleableByBookingID(ctx, bid)
	if err != nil || settleable.ID != p.ID || settleable.Status != domain.PaymentRefunded {
		t.Fatalf("get settleable(refunded): %+v, err=%v, want the same payment, status refunded", settleable, err)
	}
}

// TestPaymentLiveHoldUniquePerBookingConflicts proves the money-safety
// invariant idx_payments_live_per_booking exists to guard: two payments for
// the SAME booking can never both be authorized/capturing/voiding/captured
// at once. A sequential attempt to authorize a second payment for a booking
// that already has a live one is rejected with domain.ErrAlreadyExists — the
// real concurrent version of this is TestPaymentCompareAndSwapStatusConcurrent
// below, which proves the same thing when two requests race in parallel.
func TestPaymentLiveHoldUniquePerBookingConflicts(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p1 := newPayment(bid, rid)
	if err := repo.Create(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p1.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize p1: %v", err)
	}

	p2 := newPayment(bid, rid)
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	err := repo.CompareAndSwapStatus(ctx, p2.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("authorize p2 (second live hold for same booking) = %v, want ErrAlreadyExists", err)
	}
	// The loser's row must not have moved.
	current, gerr := repo.GetByID(ctx, p2.ID)
	if gerr != nil {
		t.Fatalf("get p2: %v", gerr)
	}
	if current.Status != domain.PaymentCreated {
		t.Fatalf("p2 status = %s, want unchanged 'created' after the lost CAS", current.Status)
	}
}

func TestPaymentCompareAndSwapStatus(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wrong expected status: rejected, no change.
	err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentFailed, time.Now())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("CAS from wrong expected status = %v, want ErrAlreadyExists", err)
	}
	current, _ := repo.GetByID(ctx, p.ID)
	if current.Status != domain.PaymentCreated {
		t.Fatalf("status changed despite a failed CAS: %s", current.Status)
	}

	// Correct expected status: succeeds and stamps the lifecycle column.
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, at); err != nil {
		t.Fatalf("CAS authorize: %v", err)
	}
	current, _ = repo.GetByID(ctx, p.ID)
	if current.Status != domain.PaymentAuthorized || current.AuthorizedAt == nil {
		t.Fatalf("CAS did not apply: %+v", current)
	}
	if !current.StatusChangedAt.Equal(at) {
		t.Errorf("status_changed_at = %v, want %v", current.StatusChangedAt, at)
	}

	// Unknown id: ErrNotFound, distinct from a status mismatch.
	if err := repo.CompareAndSwapStatus(ctx, uuid.New(), domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("CAS(missing id) = %v, want ErrNotFound", err)
	}
}

// TestPaymentCompareAndSwapStatusConcurrent is the real concurrency proof the
// task asks for: two goroutines race a genuine CAS against a live Postgres
// connection pool (not a mutex-guarded fake). Exactly one must win; the loser
// must get ErrAlreadyExists and the row must reflect only the winner's
// transition, never both, never neither.
func TestPaymentCompareAndSwapStatusConcurrent(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
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
			startWG.Wait() // maximise actual overlap against the real connection pool
			errs[i] = repo.CompareAndSwapStatus(context.Background(), p.ID,
				domain.PaymentAuthorized, domain.PaymentCapturing, time.Now())
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
		t.Fatalf("wins = %d, want exactly 1 (n=%d, losses=%d)", wins, n, losses)
	}
	if losses != n-1 {
		t.Fatalf("losses = %d, want %d", losses, n-1)
	}

	final, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status != domain.PaymentCapturing {
		t.Fatalf("final status = %s, want capturing (exactly one winner)", final.Status)
	}
}

func TestPaymentUpdateStatus(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	at := time.Now()
	if err := repo.UpdateStatus(ctx, p.ID, domain.PaymentFailed, at); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ := repo.GetByID(ctx, p.ID)
	if got.Status != domain.PaymentFailed || got.FailedAt == nil {
		t.Fatalf("update status did not apply: %+v", got)
	}
	if err := repo.UpdateStatus(ctx, uuid.New(), domain.PaymentFailed, at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update status(missing) = %v, want ErrNotFound", err)
	}
}

func TestPaymentClaimSettlement(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, time.Now()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	at := time.Now()
	if err := repo.ClaimSettlement(ctx, p.ID, "settle-key", domain.RefundTriggerNoShow, at); err != nil {
		t.Fatalf("claim settlement: %v", err)
	}
	got, _ := repo.GetByID(ctx, p.ID)
	if got.SettledAt == nil || got.SettledTrigger == nil || *got.SettledTrigger != domain.RefundTriggerNoShow {
		t.Fatalf("settlement not recorded: %+v", got)
	}
	if got.SettlementIdempotencyKey == nil || *got.SettlementIdempotencyKey != "settle-key" {
		t.Fatalf("settlement idempotency key not recorded: %+v", got)
	}

	// A second claim (different key) must be rejected — a payment settles once.
	if err := repo.ClaimSettlement(ctx, p.ID, "other-key", domain.RefundTriggerGuestCancel, time.Now()); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second claim settlement = %v, want ErrAlreadyExists", err)
	}

	if err := repo.ClaimSettlement(ctx, uuid.New(), "k", domain.RefundTriggerNoShow, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("claim settlement(missing) = %v, want ErrNotFound", err)
	}
}

func TestPaymentClaimStaleRespectsThresholdAndOrder(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := New(pool)
	txm := sqltx.NewManager(pool)

	mkStuck := func(agoHours int) *domain.Payment {
		bid := seedBooking(t, pool, rid)
		p := newPayment(bid, rid)
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
			t.Fatalf("authorize: %v", err)
		}
		claimedAt := time.Now().Add(-time.Duration(agoHours) * time.Hour)
		if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCapturing, claimedAt); err != nil {
			t.Fatalf("claim capturing: %v", err)
		}
		return p
	}

	stale := mkStuck(2) // stuck 2h ago
	fresh := mkStuck(0) // just claimed, not stale yet

	cutoff := time.Now().Add(-time.Hour)
	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimStale(ctx, []domain.PaymentStatus{domain.PaymentCapturing}, cutoff, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != stale.ID {
			t.Errorf("claim stale = %d rows, want only the genuinely stale one (fresh id=%s must not appear)", len(due), fresh.ID)
		}

		// A second worker must not see the same locked row.
		locked, err := repo.ClaimStale(context.Background(), []domain.PaymentStatus{domain.PaymentCapturing}, cutoff, 10)
		if err != nil {
			return err
		}
		if len(locked) != 0 {
			t.Errorf("second worker claimed %d locked rows, want 0", len(locked))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim tx: %v", err)
	}
}

func TestPaymentClaimExpiredHolds(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := New(pool)

	mkHold := func(expiresIn time.Duration) *domain.Payment {
		bid := seedBooking(t, pool, rid)
		p := newPayment(bid, rid)
		exp := time.Now().Add(expiresIn)
		p.ExpiresAt = &exp
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
			t.Fatalf("authorize: %v", err)
		}
		return p
	}

	expired := mkHold(-time.Hour)
	notYet := mkHold(time.Hour)

	due, err := repo.ClaimExpiredHolds(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("claim expired holds: %v", err)
	}
	if len(due) != 1 || due[0].ID != expired.ID {
		t.Fatalf("claim expired holds = %d rows, want only the expired one (not-yet id=%s must not appear)", len(due), notYet.ID)
	}
}

func TestPaymentRecordReconcileAttempt(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCapturing, time.Now()); err != nil {
		t.Fatalf("claim capturing: %v", err)
	}

	for i := 1; i <= 3; i++ {
		attempts, needsReview, err := repo.RecordReconcileAttempt(ctx, p.ID, domain.PaymentCapturing, time.Now(), 3)
		if err != nil {
			t.Fatalf("record attempt %d: %v", i, err)
		}
		if attempts != i {
			t.Errorf("attempt %d: attempts = %d, want %d", i, attempts, i)
		}
		wantReview := i >= 3
		if needsReview != wantReview {
			t.Errorf("attempt %d: needsReview = %v, want %v", i, needsReview, wantReview)
		}
	}
	got, _ := repo.GetByID(ctx, p.ID)
	if !got.NeedsManualReview || got.ReconcileAttempts != 3 {
		t.Fatalf("final state not persisted: %+v", got)
	}

	// The status moved on: a further attempt is a conflict, not a bump.
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCapturing, domain.PaymentCaptured, time.Now()); err != nil {
		t.Fatalf("resolve capturing: %v", err)
	}
	if _, _, err := repo.RecordReconcileAttempt(ctx, p.ID, domain.PaymentCapturing, time.Now(), 3); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("record attempt after resolution = %v, want ErrAlreadyExists", err)
	}
	// A real transition resets the counters.
	got, _ = repo.GetByID(ctx, p.ID)
	if got.ReconcileAttempts != 0 || got.NeedsManualReview {
		t.Fatalf("CompareAndSwapStatus did not reset reconcile bookkeeping: %+v", got)
	}

	if _, _, err := repo.RecordReconcileAttempt(ctx, uuid.New(), domain.PaymentCapturing, time.Now(), 3); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record attempt(missing) = %v, want ErrNotFound", err)
	}
}

// TestPaymentTransactionRollbackLeavesNoTrace proves the transaction boundary
// itself: a mid-transaction failure after a real status write and a real
// ledger write must leave BOTH unreverted in the database — the whole point
// of running them inside one sqltx.Manager transaction.
func TestPaymentTransactionRollbackLeavesNoTrace(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	repo := New(pool)
	ledger := NewLedger(pool)
	txm := sqltx.NewManager(pool)

	p := newPayment(bid, rid)
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentAuthorized, time.Now()); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	boom := errors.New("boom: simulated failure after the writes")
	now := time.Now()
	entries := []domain.PaymentLedgerEntry{
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountGuest, Direction: domain.DirectionDebit, AmountMinor: p.AmountMinor, Currency: p.Currency, EntryType: domain.EntryCapture, CreatedAt: now},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountRestaurant, Direction: domain.DirectionCredit, AmountMinor: p.BaseAmountMinor, Currency: p.Currency, EntryType: domain.EntryCapture, CreatedAt: now},
		{ID: uuid.New(), PaymentID: p.ID, Account: domain.AccountPlatform, Direction: domain.DirectionCredit, AmountMinor: p.FeeMinor, Currency: p.Currency, EntryType: domain.EntryServiceFee, CreatedAt: now},
	}

	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := repo.CompareAndSwapStatus(ctx, p.ID, domain.PaymentAuthorized, domain.PaymentCaptured, now); err != nil {
			return err
		}
		if err := ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		return boom // force a rollback after both writes actually ran
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx = %v, want the sentinel boom error", err)
	}

	// The status write must be gone.
	got, gerr := repo.GetByID(ctx, p.ID)
	if gerr != nil {
		t.Fatalf("get after rollback: %v", gerr)
	}
	if got.Status != domain.PaymentAuthorized {
		t.Fatalf("status = %s after rollback, want unchanged 'authorized'", got.Status)
	}

	// The ledger write must be gone too.
	rows, lerr := ledger.ListByPaymentID(ctx, p.ID)
	if lerr != nil {
		t.Fatalf("list ledger after rollback: %v", lerr)
	}
	if len(rows) != 0 {
		t.Fatalf("ledger has %d rows after rollback, want 0", len(rows))
	}
}
