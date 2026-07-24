package payout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestOwed_ComputesNetFromLedger(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)

	// Payment 1: restaurant credited 5000.
	p1 := seedPayment(t, pool, rid)
	seedLedgerEntry(t, pool, p1, domain.DirectionCredit, 5000)
	// Payment 2: credited 3000, then a 1000 refund debit against the restaurant.
	p2 := seedPayment(t, pool, rid)
	seedLedgerEntry(t, pool, p2, domain.DirectionCredit, 3000)
	seedLedgerEntry(t, pool, p2, domain.DirectionDebit, 1000)

	owed := NewOwed(pool)
	balances, err := owed.OwedForRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("expected one currency balance, got %d", len(balances))
	}
	if balances[0].AmountMinor != 7000 { // 5000 + 3000 - 1000
		t.Fatalf("expected net owed 7000, got %d", balances[0].AmountMinor)
	}
	if len(balances[0].Entries) != 3 {
		t.Fatalf("expected 3 restaurant-account entries, got %d", len(balances[0].Entries))
	}

	ids, err := owed.OwedRestaurantIDs(ctx)
	if err != nil {
		t.Fatalf("owed ids: %v", err)
	}
	// OwedRestaurantIDs is platform-wide; sibling tests' payments/ledger rows are
	// not truncated, so assert THIS restaurant is listed, not that it is alone.
	found := false
	for _, id := range ids {
		if id == rid {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be listed as owed, got %v", rid, ids)
	}
}

func TestOwed_ExcludesClaimedEntriesAndSkipsNonPositive(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	p := seedPayment(t, pool, rid)
	e1 := seedLedgerEntry(t, pool, p, domain.DirectionCredit, 5000)

	// Claim e1 into a payout.
	po := newPayout(rid, 5000)
	if err := NewPayouts(pool).Create(ctx, po); err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if err := NewItems(pool).CreateBatch(ctx, []domain.PayoutItem{{
		PayoutID: po.ID, LedgerEntryID: e1, RestaurantID: rid, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT,
	}}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	balances, err := NewOwed(pool).OwedForRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if len(balances) != 0 {
		t.Fatalf("a claimed entry must not be owed again, got %+v", balances)
	}
	// And this restaurant no longer shows up as owed. (OwedRestaurantIDs is
	// platform-wide and payments/ledger rows from sibling tests are not
	// truncated, so assert on THIS restaurant's absence, not a global count.)
	ids, _ := NewOwed(pool).OwedRestaurantIDs(ctx)
	for _, id := range ids {
		if id == rid {
			t.Fatalf("a fully-claimed restaurant must not be listed as owed")
		}
	}
}

func TestItems_LedgerEntryNeverClaimedTwice(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	p := seedPayment(t, pool, rid)
	e1 := seedLedgerEntry(t, pool, p, domain.DirectionCredit, 5000)

	items := NewItems(pool)
	payouts := NewPayouts(pool)

	po1 := newPayout(rid, 5000)
	if err := payouts.Create(ctx, po1); err != nil {
		t.Fatalf("create payout 1: %v", err)
	}
	if err := items.CreateBatch(ctx, []domain.PayoutItem{{
		PayoutID: po1.ID, LedgerEntryID: e1, RestaurantID: rid, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT,
	}}); err != nil {
		t.Fatalf("first claim must succeed: %v", err)
	}

	po2 := newPayout(rid, 5000)
	if err := payouts.Create(ctx, po2); err != nil {
		t.Fatalf("create payout 2: %v", err)
	}
	err := items.CreateBatch(ctx, []domain.PayoutItem{{
		PayoutID: po2.ID, LedgerEntryID: e1, RestaurantID: rid, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT,
	}})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("claiming an entry twice must be ErrAlreadyExists, got %v", err)
	}

	// After a failed payout releases its claim, the entry is claimable again.
	if err := items.DeleteByPayout(ctx, po1.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := items.CreateBatch(ctx, []domain.PayoutItem{{
		PayoutID: po2.ID, LedgerEntryID: e1, RestaurantID: rid, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT,
	}}); err != nil {
		t.Fatalf("re-claim after release must succeed: %v", err)
	}
}

func TestPayouts_CASAndIdempotencyKey(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	payouts := NewPayouts(pool)

	po := newPayout(rid, 5000)
	if err := payouts.Create(ctx, po); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate idempotency key rejected.
	dup := newPayout(rid, 5000)
	dup.IdempotencyKey = po.IdempotencyKey
	if err := payouts.Create(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate idempotency key must be ErrAlreadyExists, got %v", err)
	}

	now := time.Now()
	ref := "prov-1"
	// pending -> sent.
	if err := payouts.CompareAndSwapStatus(ctx, po.ID, domain.PayoutPending, domain.PayoutSent, domain.PayoutStatusPatch{}, now); err != nil {
		t.Fatalf("cas pending->sent: %v", err)
	}
	// A second pending->sent loses (already moved).
	if err := payouts.CompareAndSwapStatus(ctx, po.ID, domain.PayoutPending, domain.PayoutSent, domain.PayoutStatusPatch{}, now); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second cas must be ErrAlreadyExists, got %v", err)
	}
	// sent -> paid with provider ref.
	if err := payouts.CompareAndSwapStatus(ctx, po.ID, domain.PayoutSent, domain.PayoutPaid, domain.PayoutStatusPatch{ProviderRef: &ref}, now); err != nil {
		t.Fatalf("cas sent->paid: %v", err)
	}
	got, err := payouts.GetByID(ctx, po.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PayoutPaid {
		t.Fatalf("expected paid, got %s", got.Status)
	}
	if got.ProviderRef == nil || *got.ProviderRef != ref {
		t.Fatalf("expected provider ref stored, got %v", got.ProviderRef)
	}
	if got.PaidAt == nil {
		t.Fatalf("paid_at must be stamped")
	}

	// CAS on a missing id -> ErrNotFound.
	if err := payouts.CompareAndSwapStatus(ctx, uuid.New(), domain.PayoutPending, domain.PayoutSent, domain.PayoutStatusPatch{}, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cas on missing id must be ErrNotFound, got %v", err)
	}
}

func TestPayouts_ClaimStaleSentOnly(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	payouts := NewPayouts(pool)

	// A stale sent payout.
	stale := newPayout(rid, 5000)
	stale.Status = domain.PayoutSent
	stale.StatusChangedAt = time.Now().Add(-time.Hour)
	if err := payouts.Create(ctx, stale); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	// A fresh pending payout (must not be claimed).
	fresh := newPayout(rid, 5000)
	if err := payouts.Create(ctx, fresh); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	var due []domain.Payout
	if err := txm(pool).WithinTx(ctx, func(ctx context.Context) error {
		var e error
		due, e = payouts.ClaimStale(ctx, []domain.PayoutStatus{domain.PayoutSent}, time.Now().Add(-time.Minute), 10)
		return e
	}); err != nil {
		t.Fatalf("claim stale: %v", err)
	}
	if len(due) != 1 || due[0].ID != stale.ID {
		t.Fatalf("expected only the stale sent payout, got %d", len(due))
	}
}

func TestDestinations_UpsertRoundtripNoRawPAN(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	dest := NewDestinations(pool)

	d := &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
		ProviderCustomerRef: "fp-user-1", MaskedIdentifier: "440043******1234",
	}
	if err := dest.Upsert(ctx, d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := dest.Get(ctx, rid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Token != d.Token || got.ProviderCustomerRef != "fp-user-1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// Replace in place (one row per restaurant).
	d2 := *d
	d2.Token = uuid.NewString()
	d2.MaskedIdentifier = "555555******9999"
	if err := dest.Upsert(ctx, &d2); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got2, _ := dest.Get(ctx, rid)
	if got2.MaskedIdentifier != "555555******9999" {
		t.Fatalf("upsert must replace in place, got %q", got2.MaskedIdentifier)
	}
}
