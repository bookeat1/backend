package payment

import (
	"errors"
	"testing"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// NOTE: these tests truncate restaurant_split_accounts only. They must NOT
// truncate payment_providers — migration 0007 seeds the acquirer rows and other
// packages' tests depend on them existing (the standing test-DB pollution trap
// this repo has been bitten by once already).

func TestSplitAccountsUpsertAndGetActive(t *testing.T) {
	pool, ctx := setup(t)
	testdb.Truncate(t, pool, "restaurant_split_accounts")
	repo := NewSplitAccounts(pool)
	restaurantID := seedRestaurant(t, pool)

	// A venue that was never onboarded is ErrNotFound — the state EVERY venue
	// is in until acquiring issues it a sub-merchant id.
	if _, err := repo.GetActive(ctx, domain.ProviderTipTopPay, restaurantID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActive() on a venue with no account = %v, want ErrNotFound", err)
	}

	account := &domain.RestaurantSplitAccount{
		RestaurantID: restaurantID,
		Provider:     domain.ProviderTipTopPay,
		AccountRef:   "  test_api_00000000000000000000003  ", // padded on purpose
		IsActive:     true,
	}
	if err := repo.Upsert(ctx, account); err != nil {
		t.Fatalf("Upsert() = %v", err)
	}
	if account.AccountRef != "test_api_00000000000000000000003" {
		t.Fatalf("stored ref = %q, want it trimmed", account.AccountRef)
	}

	got, err := repo.GetActive(ctx, domain.ProviderTipTopPay, restaurantID)
	if err != nil {
		t.Fatalf("GetActive() = %v", err)
	}
	if got.AccountRef != "test_api_00000000000000000000003" || !got.IsActive {
		t.Fatalf("got = %+v", got)
	}

	// Re-onboarding the same venue replaces the id rather than creating a
	// second row (the primary key is the pair).
	account.AccountRef = "test_api_00000000000000000000009"
	if err := repo.Upsert(ctx, account); err != nil {
		t.Fatalf("Upsert() second time = %v", err)
	}
	got, err = repo.GetActive(ctx, domain.ProviderTipTopPay, restaurantID)
	if err != nil {
		t.Fatalf("GetActive() after re-upsert = %v", err)
	}
	if got.AccountRef != "test_api_00000000000000000000009" {
		t.Fatalf("ref = %q, want the new one", got.AccountRef)
	}

	// A deactivated account reads exactly like a missing one: the checkout must
	// refuse a split payment in both cases.
	account.IsActive = false
	if err := repo.Upsert(ctx, account); err != nil {
		t.Fatalf("Upsert(inactive) = %v", err)
	}
	if _, err := repo.GetActive(ctx, domain.ProviderTipTopPay, restaurantID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetActive() on a deactivated account = %v, want ErrNotFound", err)
	}
}

// TestSplitAccountsOneAccountCannotServeTwoVenues pins the constraint that
// stops one venue from collecting another's money.
func TestSplitAccountsOneAccountCannotServeTwoVenues(t *testing.T) {
	pool, ctx := setup(t)
	testdb.Truncate(t, pool, "restaurant_split_accounts")
	repo := NewSplitAccounts(pool)

	first := seedRestaurant(t, pool)
	second := seedRestaurant(t, pool)
	const ref = "test_api_shared_account"

	if err := repo.Upsert(ctx, &domain.RestaurantSplitAccount{
		RestaurantID: first, Provider: domain.ProviderTipTopPay, AccountRef: ref, IsActive: true,
	}); err != nil {
		t.Fatalf("Upsert(first) = %v", err)
	}

	err := repo.Upsert(ctx, &domain.RestaurantSplitAccount{
		RestaurantID: second, Provider: domain.ProviderTipTopPay, AccountRef: ref, IsActive: true,
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Upsert(second venue, same account) = %v, want ErrAlreadyExists", err)
	}
}

// TestSplitAccountsRejectABlankIdentifier: a blank string would look configured
// and pay nobody. Guarded in Go AND by a CHECK constraint; this asserts the Go
// side answers before the round trip.
func TestSplitAccountsRejectABlankIdentifier(t *testing.T) {
	pool, ctx := setup(t)
	testdb.Truncate(t, pool, "restaurant_split_accounts")
	repo := NewSplitAccounts(pool)

	err := repo.Upsert(ctx, &domain.RestaurantSplitAccount{
		RestaurantID: seedRestaurant(t, pool), Provider: domain.ProviderTipTopPay, AccountRef: "   ", IsActive: true,
	})
	if err == nil {
		t.Fatalf("Upsert(blank ref) = nil, want a rejection")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeSplitAccountMissing {
		t.Fatalf("code = %q, want %q", code, domain.CodeSplitAccountMissing)
	}
}

// TestSplitAccountsUnknownProviderIsRejectedByTheDatabase proves the FK is
// doing its job: a typo cannot create a mapping for an acquirer that does not
// exist.
func TestSplitAccountsUnknownProviderIsRejectedByTheDatabase(t *testing.T) {
	pool, ctx := setup(t)
	testdb.Truncate(t, pool, "restaurant_split_accounts")

	_, err := pool.Exec(ctx,
		`INSERT INTO restaurant_split_accounts (restaurant_id, provider, account_ref)
		 VALUES ($1, 'tiptopay', 'typo')`, seedRestaurant(t, pool))
	if err == nil {
		t.Fatalf("insert with an unknown provider succeeded")
	}
}
