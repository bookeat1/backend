package payout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
)

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

// seedUser inserts a platform user to act as the author of a policy change —
// updated_by is a real FK, so the audit column has to reference a real row.
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Admin')`,
		id, id.String()+"@example.test", "+7700"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestSettings_RoundTripKeepsTheThreeStates is what the whole per-venue policy
// rests on: SQL NULL and an explicit 0 must come back as two DIFFERENT things.
// If NULL round-tripped as 0 a venue with no threshold override would silently
// be paid any positive balance, fee and all.
func TestSettings_RoundTripKeepsTheThreeStates(t *testing.T) {
	pool, ctx := setup(t)
	repo := NewSettings(pool)

	// A: both overridden, one of them to an explicit zero.
	ridA := seedRestaurant(t, pool)
	if err := repo.Upsert(ctx, &domain.PayoutSettings{
		RestaurantID: ridA, MinPayoutMinor: ptrInt64(0), MaxHoldDays: ptrInt(3),
	}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	got, err := repo.Get(ctx, ridA)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if got.MinPayoutMinor == nil || *got.MinPayoutMinor != 0 {
		t.Fatalf("an explicit zero threshold must survive the round trip, got %v", got.MinPayoutMinor)
	}
	if got.MaxHoldDays == nil || *got.MaxHoldDays != 3 {
		t.Fatalf("expected max_hold_days=3, got %v", got.MaxHoldDays)
	}

	// B: a row that overrides only the hold window.
	ridB := seedRestaurant(t, pool)
	if err := repo.Upsert(ctx, &domain.PayoutSettings{RestaurantID: ridB, MaxHoldDays: ptrInt(10)}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	got, err = repo.Get(ctx, ridB)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if got.MinPayoutMinor != nil {
		t.Fatalf("an absent threshold must stay NULL, not 0, got %d", *got.MinPayoutMinor)
	}

	// C: no row at all — the common case.
	if _, err := repo.Get(ctx, seedRestaurant(t, pool)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a venue with no policy row must be ErrNotFound, got %v", err)
	}
}

// TestSettings_UpsertClearsAnOverrideInPlace: putting a venue back on the
// platform default is a legitimate edit and must actually write NULL, not keep
// the previous value.
func TestSettings_UpsertClearsAnOverrideInPlace(t *testing.T) {
	pool, ctx := setup(t)
	repo := NewSettings(pool)
	rid := seedRestaurant(t, pool)
	author := seedUser(t, pool)

	if err := repo.Upsert(ctx, &domain.PayoutSettings{
		RestaurantID: rid, MinPayoutMinor: ptrInt64(500_000), MaxHoldDays: ptrInt(3), UpdatedBy: &author,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.Upsert(ctx, &domain.PayoutSettings{RestaurantID: rid}); err != nil {
		t.Fatalf("clearing upsert: %v", err)
	}
	got, err := repo.Get(ctx, rid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MinPayoutMinor != nil || got.MaxHoldDays != nil {
		t.Fatalf("both overrides must be cleared, got %+v", got)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM restaurant_payout_settings WHERE restaurant_id=$1`, rid).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("one row per venue, got %d", rows)
	}
}

// TestSettings_DatabaseRefusesAnAbsurdPolicy: the Go validation is not the only
// guard. This row is exactly the kind an operator edits by hand.
func TestSettings_DatabaseRefusesAnAbsurdPolicy(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_payout_settings (restaurant_id, min_payout_minor) VALUES ($1, -1)`,
		rid); err == nil {
		t.Fatal("a negative payout threshold must be refused by the database")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_payout_settings (restaurant_id, max_hold_days) VALUES ($1, 4000)`,
		rid); err == nil {
		t.Fatal("a hold window of 4000 days must be refused by the database")
	}
}

// TestSettings_ForRestaurantsBatchesAndOmitsUnconfiguredVenues: the daily pass
// resolves every owed venue in one read, and a venue without a row must be
// ABSENT rather than present with zero values.
func TestSettings_ForRestaurantsBatchesAndOmitsUnconfiguredVenues(t *testing.T) {
	pool, ctx := setup(t)
	repo := NewSettings(pool)

	configured := seedRestaurant(t, pool)
	plain := seedRestaurant(t, pool)
	if err := repo.Upsert(ctx, &domain.PayoutSettings{
		RestaurantID: configured, MinPayoutMinor: ptrInt64(250_000),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.ForRestaurants(ctx, []uuid.UUID{configured, plain, uuid.New()})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("only configured venues may appear, got %d entries", len(got))
	}
	if s := got[configured]; s.MinPayoutMinor == nil || *s.MinPayoutMinor != 250_000 {
		t.Fatalf("unexpected settings for the configured venue: %+v", s)
	}
	if _, present := got[plain]; present {
		t.Fatal("a venue with no policy row must be absent from the map, not zero-valued")
	}

	// An empty request must not hit the database at all.
	empty, err := repo.ForRestaurants(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty batch: %v / %d", err, len(empty))
	}
}

// TestSettings_DeletingAVenueRemovesItsPolicy: ON DELETE CASCADE, so a deleted
// venue cannot leave an orphan money knob behind.
func TestSettings_DeletingAVenueRemovesItsPolicy(t *testing.T) {
	pool, ctx := setup(t)
	repo := NewSettings(pool)
	rid := seedRestaurant(t, pool)
	if err := repo.Upsert(ctx, &domain.PayoutSettings{RestaurantID: rid, MaxHoldDays: ptrInt(5)}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM restaurants WHERE id=$1`, rid); err != nil {
		t.Fatalf("delete restaurant: %v", err)
	}
	if _, err := repo.Get(ctx, rid); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the policy must be gone with the venue, got %v", err)
	}
}

// TestPayouts_ForcedByAgeRoundTrips: a forced payout must be recognisable on
// the row it was written to, otherwise the venue's statement cannot explain why
// a small payout paid the acquirer's floor.
func TestPayouts_ForcedByAgeRoundTrips(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := NewPayouts(pool)

	forced := scheduledPayout(rid, 40_000, utcDay(2026, 7, 31))
	forced.ForcedByAge = true
	if err := repo.Create(ctx, forced); err != nil {
		t.Fatalf("create forced payout: %v", err)
	}
	normal := scheduledPayout(rid, 2_000_000, utcDay(2026, 8, 1))
	if err := repo.Create(ctx, normal); err != nil {
		t.Fatalf("create normal payout: %v", err)
	}

	got, err := repo.GetByID(ctx, forced.ID)
	if err != nil {
		t.Fatalf("get forced: %v", err)
	}
	if !got.ForcedByAge {
		t.Fatal("the age-forced flag must survive the write/read cycle")
	}
	got, err = repo.GetByID(ctx, normal.ID)
	if err != nil {
		t.Fatalf("get normal: %v", err)
	}
	if got.ForcedByAge {
		t.Fatal("a threshold-driven payout must not be reported as forced by age")
	}
}

// TestOwed_CarriesTheAgeOfTheMoney: the max-hold rule is measured against the
// oldest unclaimed ledger entry, so the owed read has to carry when each entry
// was booked. Without it the daily pass can never know a venue's money is old.
func TestOwed_CarriesTheAgeOfTheMoney(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	paymentID := seedPayment(t, pool, rid)

	first := seedLedgerEntry(t, pool, paymentID, domain.DirectionCredit, 40_000)
	// Backdate the first entry by 10 days — money that has been waiting.
	if _, err := pool.Exec(ctx,
		`UPDATE payment_ledger_entries SET created_at = now() - interval '10 days' WHERE id=$1`,
		first); err != nil {
		t.Fatalf("backdate entry: %v", err)
	}
	seedLedgerEntry(t, pool, paymentID, domain.DirectionCredit, 10_000)

	balances, err := NewOwed(pool).OwedForRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("expected one currency, got %d", len(balances))
	}
	for _, e := range balances[0].Entries {
		if e.CreatedAt.IsZero() {
			t.Fatalf("every owed entry must carry its booking instant, got zero for %s", e.LedgerEntryID)
		}
	}
	oldest := balances[0].OldestEntryAt()
	if oldest.IsZero() {
		t.Fatal("the balance must expose the age of its oldest money")
	}
	if age := time.Since(oldest); age < 9*24*time.Hour {
		t.Fatalf("expected the oldest entry to be ~10 days old, got %s", age)
	}
}
