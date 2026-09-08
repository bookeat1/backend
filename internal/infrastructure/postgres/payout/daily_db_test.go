package payout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
)

// utcDay is the calendar label a scheduled payout carries: a venue-local date
// normalised to UTC midnight.
func utcDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// scheduledPayout builds a payout that claims a venue-local day.
func scheduledPayout(restaurantID uuid.UUID, amountMinor int64, day time.Time) *domain.Payout {
	p := newPayout(restaurantID, amountMinor)
	p.GrossAmountMinor = amountMinor
	p.FeeMinor = 30_000
	p.FeeBearer = domain.PayoutFeeBearerPlatform
	end := day.Add(24 * time.Hour)
	p.PeriodDate, p.PeriodEndAt = &day, &end
	return p
}

// TestPayouts_OnePerVenuePerPeriod is the once-per-day guarantee AT THE
// DATABASE — the layer the whole design rests on. Not a usecase check: two
// processes that both believe they should pay a venue must collide here.
//
// MUTATION CHECK: dropping uq_payouts_venue_period from migration 0052 makes
// the second insert succeed and this test fail.
func TestPayouts_OnePerVenuePerPeriod(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)
	day := utcDay(2026, 7, 24)

	if err := repo.Create(ctx, scheduledPayout(rid, 1_000_000, day)); err != nil {
		t.Fatalf("first payout for the day: %v", err)
	}
	err := repo.Create(ctx, scheduledPayout(rid, 2_000_000, day))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("a second live payout for the same venue-day must be refused by the DB, got %v", err)
	}

	// A DIFFERENT day is fine — the constraint is per period, not per venue.
	if err := repo.Create(ctx, scheduledPayout(rid, 1_000_000, utcDay(2026, 7, 25))); err != nil {
		t.Fatalf("the next day must be payable: %v", err)
	}
	// A MANUAL payout (NULL period) is unconstrained: NULLs are distinct.
	if err := repo.Create(ctx, newPayout(rid, 500_000)); err != nil {
		t.Fatalf("a manual payout must not be blocked by the period index: %v", err)
	}
}

// TestPayouts_FailedPayoutReleasesThePeriod: the partial index excludes failed
// rows, so a declined send frees the venue-day for a retry. Without that, a
// single decline would silently postpone a venue's money by a day.
//
// MUTATION CHECK: removing `WHERE status <> 'failed'` from the index makes the
// retry fail.
func TestPayouts_FailedPayoutReleasesThePeriod(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)
	day := utcDay(2026, 7, 24)

	first := scheduledPayout(rid, 1_000_000, day)
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}
	// pending -> sent -> failed, the real path a declined payout takes.
	now := time.Now()
	if err := repo.CompareAndSwapStatus(ctx, first.ID, domain.PayoutPending, domain.PayoutSent, domain.PayoutStatusPatch{}, now); err != nil {
		t.Fatalf("cas to sent: %v", err)
	}
	if err := repo.CompareAndSwapStatus(ctx, first.ID, domain.PayoutSent, domain.PayoutFailed, domain.PayoutStatusPatch{}, now); err != nil {
		t.Fatalf("cas to failed: %v", err)
	}

	if err := repo.Create(ctx, scheduledPayout(rid, 1_000_000, day)); err != nil {
		t.Fatalf("a failed payout must release its period for a retry, got %v", err)
	}
}

// TestPayouts_ConcurrentInsertsForTheSameDay is the two-worker race against a
// real Postgres, where the unique index — not any application lock — is the
// arbiter.
func TestPayouts_ConcurrentInsertsForTheSameDay(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)
	day := utcDay(2026, 7, 24)

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, conflicts int
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.Create(ctx, scheduledPayout(rid, 1_000_000, day))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrAlreadyExists):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok != 1 || conflicts != workers-1 {
		t.Fatalf("expected exactly 1 winner and %d conflicts, got %d/%d", workers-1, ok, conflicts)
	}
}

// TestPayouts_FeeAndPeriodRoundTrip: the fee must survive a write/read cycle,
// otherwise "what did this payout cost us" is unanswerable from the row.
func TestPayouts_FeeAndPeriodRoundTrip(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)
	day := utcDay(2026, 7, 24)

	p := scheduledPayout(rid, 1_000_000, day)
	p.AmountMinor = 970_000 // venue-bears policy
	p.FeeBearer = domain.PayoutFeeBearerVenue
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GrossAmountMinor != 1_000_000 || got.FeeMinor != 30_000 || got.AmountMinor != 970_000 {
		t.Fatalf("amounts did not round-trip: gross=%d fee=%d amount=%d",
			got.GrossAmountMinor, got.FeeMinor, got.AmountMinor)
	}
	if got.FeeBearer != domain.PayoutFeeBearerVenue {
		t.Fatalf("fee bearer did not round-trip: %q", got.FeeBearer)
	}
	if got.PeriodDate == nil || !got.PeriodDate.Equal(day) {
		t.Fatalf("period date did not round-trip: %v", got.PeriodDate)
	}
}

// TestOwed_UpToRespectsThePeriodEnd: the venue-local day boundary as SQL. An
// entry created at or after the cutoff belongs to the NEXT period.
func TestOwed_UpToRespectsThePeriodEnd(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	pid := seedPayment(t, pool, rid)

	older := seedLedgerEntry(t, pool, pid, domain.DirectionCredit, 4000)
	newer := seedLedgerEntry(t, pool, pid, domain.DirectionCredit, 6000)
	// The helper stamps created_at with now(); move the two entries onto known
	// instants either side of a cutoff.
	cutoff := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	setEntryTime(t, pool, older, cutoff.Add(-time.Hour))
	setEntryTime(t, pool, newer, cutoff.Add(time.Hour))

	owed := NewOwed(pool)
	bounded, err := owed.OwedForRestaurantUpTo(ctx, rid, cutoff)
	if err != nil {
		t.Fatalf("owed up to: %v", err)
	}
	if len(bounded) != 1 || bounded[0].AmountMinor != 4000 {
		t.Fatalf("expected only the pre-cutoff 4000, got %+v", bounded)
	}

	unbounded, err := owed.OwedForRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if len(unbounded) != 1 || unbounded[0].AmountMinor != 10000 {
		t.Fatalf("the unbounded read must still see everything (10000), got %+v", unbounded)
	}
}

// TestPayoutLedger_AppendsAndRefusesADuplicate covers the fee's accounting
// mirror and its replay guard.
func TestPayoutLedger_AppendsAndRefusesADuplicate(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)
	p := scheduledPayout(rid, 1_000_000, utcDay(2026, 7, 24))
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create payout: %v", err)
	}

	ledger := NewLedger(pool)
	entries := domain.PayoutLedgerEntries(*p, time.Now())
	if err := domain.ValidatePayoutLedgerBalance(entries); err != nil {
		t.Fatalf("batch does not balance: %v", err)
	}
	if err := ledger.CreateBatch(ctx, entries); err != nil {
		t.Fatalf("create batch: %v", err)
	}

	stored, err := ledger.ListByPayout(ctx, p.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 4 {
		t.Fatalf("expected 4 lines (settlement pair + fee pair), got %d", len(stored))
	}
	var platformFee int64
	for _, e := range stored {
		if e.Account == domain.AccountPlatform && e.EntryType == domain.EntryPayoutFee {
			platformFee += e.AmountMinor
		}
	}
	if platformFee != 30_000 {
		t.Fatalf("the platform-borne fee must be readable from the ledger, got %d", platformFee)
	}

	// A replay must not book the cost twice.
	replay := domain.PayoutLedgerEntries(*p, time.Now())
	if err := ledger.CreateBatch(ctx, replay); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("a replayed ledger batch must be refused, got %v", err)
	}
}

// TestVenues_TimezonesForBatchesAndOmitsUnset pins the timezone reader's
// contract: a venue with no zone is ABSENT, so the caller applies the platform
// fallback explicitly instead of this layer guessing.
func TestVenues_TimezonesForBatchesAndOmitsUnset(t *testing.T) {
	pool, ctx := setup(t)
	withTZ := seedRestaurant(t, pool)
	withoutTZ := seedRestaurant(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET timezone='Europe/Istanbul' WHERE id=$1`, withTZ); err != nil {
		t.Fatalf("set timezone: %v", err)
	}

	got, err := NewVenues(pool).TimezonesFor(ctx, []uuid.UUID{withTZ, withoutTZ})
	if err != nil {
		t.Fatalf("timezones: %v", err)
	}
	if got[withTZ] != "Europe/Istanbul" {
		t.Fatalf("expected the venue's own zone, got %q", got[withTZ])
	}
	if _, present := got[withoutTZ]; present {
		t.Fatalf("a venue without a timezone must be absent from the map, got %q", got[withoutTZ])
	}
}

// setEntryTime moves a ledger entry onto a known instant. Test-only: the ledger
// is append-only in production and nothing outside tests updates it.
func setEntryTime(t *testing.T, pool *pgxpool.Pool, entryID uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_ledger_entries SET created_at=$2 WHERE id=$1`, entryID, at); err != nil {
		t.Fatalf("set entry time: %v", err)
	}
}
