package payouts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func cardTokenInput() DestinationInput {
	return DestinationInput{
		Method:              domain.PayoutMethodFreedomPayCardToken,
		Token:               uuid.NewString(),
		ProviderCustomerRef: "fp-user-42",
		MaskedIdentifier:    "440043******1234",
	}
}

// --- RBAC on setting the destination ---------------------------------------

func TestSetDestination_RBAC(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()

	t.Run("owner/manager (restaurant.manage) may set it", func(t *testing.T) {
		h := newHarness()
		h.perms.allow = true // ManagerUseCase would return true for owner/manager
		if _, err := h.uc.SetDestination(ctx, staff(), rid, cardTokenInput()); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.perms.gotPerm != domain.PermRestaurantManage {
			t.Fatalf("expected gate on %q, got %q", domain.PermRestaurantManage, h.perms.gotPerm)
		}
		if h.perms.gotRest != rid {
			t.Fatalf("permission must be checked against the path restaurant, got %s", h.perms.gotRest)
		}
		if h.dest.upserts != 1 {
			t.Fatalf("expected one upsert, got %d", h.dest.upserts)
		}
	})

	t.Run("hostess (no restaurant.manage) is forbidden", func(t *testing.T) {
		h := newHarness()
		h.perms.allow = false // ManagerUseCase returns false — hostess lacks restaurant.manage
		_, err := h.uc.SetDestination(ctx, staff(), rid, cardTokenInput())
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("expected ErrForbidden, got %v", err)
		}
		if h.dest.upserts != 0 {
			t.Fatalf("nothing must be written on a forbidden call, got %d upserts", h.dest.upserts)
		}
		// Nail the "hostess" claim against the real RBAC matrix.
		if domain.StaffRoleHostess.HasPermission(domain.PermRestaurantManage) {
			t.Fatal("matrix regression: a hostess must NOT hold restaurant.manage")
		}
	})

	t.Run("superadmin bypasses the per-restaurant check", func(t *testing.T) {
		h := newHarness()
		h.perms.allow = false
		if _, err := h.uc.SetDestination(ctx, superadmin(), rid, cardTokenInput()); err != nil {
			t.Fatalf("superadmin should pass, got %v", err)
		}
		if h.perms.calls != 0 {
			t.Fatalf("superadmin must not consult the per-restaurant checker, got %d calls", h.perms.calls)
		}
	})
}

// --- PCI: no raw PAN is ever stored ----------------------------------------

func TestSetDestination_RejectsRawPAN(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()

	cases := map[string]DestinationInput{
		"PAN as token": {
			Method:              domain.PayoutMethodFreedomPayCardToken,
			Token:               "4400430000001234",
			ProviderCustomerRef: "fp-user-1",
			MaskedIdentifier:    "440043******1234",
		},
		"spaced PAN as token": {
			Method:              domain.PayoutMethodFreedomPayCardToken,
			Token:               "4400 4300 0000 1234",
			ProviderCustomerRef: "fp-user-1",
		},
		"full PAN in the masked field": {
			Method:              domain.PayoutMethodFreedomPayCardToken,
			Token:               uuid.NewString(),
			ProviderCustomerRef: "fp-user-1",
			MaskedIdentifier:    "4400430000001234",
		},
		"non-token garbage": {
			Method:              domain.PayoutMethodFreedomPayCardToken,
			Token:               "not-a-token",
			ProviderCustomerRef: "fp-user-1",
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			_, err := h.uc.SetDestination(ctx, superadmin(), rid, in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
			if h.dest.upserts != 0 {
				t.Fatalf("a raw PAN must never be written; got %d upserts", h.dest.upserts)
			}
		})
	}
}

// --- generation: owed amount + one-payout-per-entry -------------------------

func seedDestination(h *harness, rid uuid.UUID) {
	_ = h.dest.Upsert(context.Background(), &domain.PayoutDestination{
		RestaurantID:        rid,
		Provider:            domain.ProviderFreedomPay,
		Method:              domain.PayoutMethodFreedomPayCardToken,
		Token:               uuid.NewString(),
		ProviderCustomerRef: "fp-user-1",
	})
}

func TestGenerate_ComputesAmountAndClaimsEntries(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)

	e1, e2, e3 := uuid.New(), uuid.New(), uuid.New()
	// 5000 credit + 3000 credit - 1000 debit = 7000 net owed.
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid,
		Currency:     domain.CurrencyKZT,
		AmountMinor:  7000,
		Entries: []domain.OwedEntry{
			{LedgerEntryID: e1, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT},
			{LedgerEntryID: e2, AmountSignedMinor: 3000, Currency: domain.CurrencyKZT},
			{LedgerEntryID: e3, AmountSignedMinor: -1000, Currency: domain.CurrencyKZT},
		},
	}}

	created, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 payout, got %d", len(created))
	}
	if created[0].AmountMinor != 7000 {
		t.Fatalf("expected amount 7000, got %d", created[0].AmountMinor)
	}
	if created[0].Status != domain.PayoutPending {
		t.Fatalf("expected pending, got %s", created[0].Status)
	}
	items, _ := h.items.ListByPayout(ctx, created[0].ID)
	if len(items) != 3 {
		t.Fatalf("expected 3 claimed entries, got %d", len(items))
	}
}

func TestGenerate_LedgerEntryNeverInTwoPayouts(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)

	e1 := uuid.New()
	balance := []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: 5000,
		Entries: []domain.OwedEntry{{LedgerEntryID: e1, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT}},
	}}
	h.owed.byRestaurant[rid] = balance

	first, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err != nil || len(first) != 1 {
		t.Fatalf("first generate: %v (n=%d)", err, len(first))
	}
	// The owed reader still reports the same entry (a real reader would exclude
	// it; here we simulate the race where two generations see it unclaimed).
	second, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err != nil {
		t.Fatalf("second generate must not error, it should skip: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("the already-claimed entry must not be paid a second time; got %d payouts", len(second))
	}
	// Exactly one payout owns the entry.
	if owner := h.items.byEntry[e1]; owner != first[0].ID {
		t.Fatalf("entry must stay owned by the first payout")
	}
}

// --- double-send must not double-pay ---------------------------------------

func TestSendPayout_DoubleSendDoesNotDoublePay(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: 5000,
		Entries: []domain.OwedEntry{{LedgerEntryID: uuid.New(), AmountSignedMinor: 5000, Currency: domain.CurrencyKZT}},
	}}
	created, _ := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	id := created[0].ID

	first, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if first.Status != domain.PayoutPaid {
		t.Fatalf("expected paid, got %s", first.Status)
	}
	second, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if second.Status != domain.PayoutPaid {
		t.Fatalf("expected still paid, got %s", second.Status)
	}
	if h.gw.payoutCalls != 1 {
		t.Fatalf("acquirer must be called exactly once across a double send, got %d", h.gw.payoutCalls)
	}
}

func TestSendPayout_AlreadySentIsNotResent(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	// A payout already claimed into `sent` (e.g. a prior dispatch whose answer
	// was lost) must never be dispatched again by SendPayout.
	id := uuid.New()
	_ = h.payouts.Create(ctx, &domain.Payout{
		ID: id, RestaurantID: rid, AmountMinor: 5000, Currency: domain.CurrencyKZT,
		Status: domain.PayoutSent, Method: domain.PayoutMethodFreedomPayCardToken,
		IdempotencyKey: "payout:" + id.String(), StatusChangedAt: time.Now(),
	})
	got, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Status != domain.PayoutSent {
		t.Fatalf("expected sent, got %s", got.Status)
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("a sent payout must not be re-dispatched, got %d calls", h.gw.payoutCalls)
	}
}

func TestSendPayout_DeclineReleasesClaim(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	entry := uuid.New()
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: 5000,
		Entries: []domain.OwedEntry{{LedgerEntryID: entry, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT}},
	}}
	created, _ := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	id := created[0].ID

	// A definite decline: the payout fails and its claim is released.
	h.gw.payoutFn = func(domain.PayoutRequest) (*domain.GatewayPayout, error) {
		return nil, domain.ErrProviderDeclined
	}
	got, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Status != domain.PayoutFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	if _, stillClaimed := h.items.byEntry[entry]; stillClaimed {
		t.Fatal("a failed payout must release its ledger entries so they are owed again")
	}
}

func TestSendPayout_UnknownOutcomeLeavesSent(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	entry := uuid.New()
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: 5000,
		Entries: []domain.OwedEntry{{LedgerEntryID: entry, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT}},
	}}
	created, _ := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	id := created[0].ID

	h.gw.payoutFn = func(domain.PayoutRequest) (*domain.GatewayPayout, error) {
		return nil, domain.ErrProviderOutcomeUnknown
	}
	_, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err == nil {
		t.Fatal("an unknown outcome must surface as an error to the caller")
	}
	cur, _ := h.payouts.GetByID(ctx, id)
	if cur.Status != domain.PayoutSent {
		t.Fatalf("an unknown outcome must leave the payout sent, got %s", cur.Status)
	}
	if _, stillClaimed := h.items.byEntry[entry]; !stillClaimed {
		t.Fatal("an unknown outcome must NOT release the claim (money may have moved)")
	}
}

func TestSendPayout_RBACSuperadminOnly(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	_, err := h.uc.SendPayout(ctx, staff(), uuid.New())
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a non-superadmin must be forbidden from sending payouts, got %v", err)
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("no acquirer call on a forbidden send, got %d", h.gw.payoutCalls)
	}
}

// --- reconciliation resolves a stuck payout --------------------------------

func TestReconcile_ResolvesStuckPayoutToPaid(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	pays := newFakePayouts()
	items := newFakeItems()
	gw := &fakeGateway{}

	id := uuid.New()
	// A payout stranded in `sent` for longer than StuckAfter.
	_ = pays.Create(ctx, &domain.Payout{
		ID: id, RestaurantID: rid, AmountMinor: 5000, Currency: domain.CurrencyKZT,
		Status: domain.PayoutSent, Method: domain.PayoutMethodFreedomPayCardToken,
		IdempotencyKey:  "payout:" + id.String(),
		StatusChangedAt: time.Now().Add(-time.Hour),
	})
	gw.getFn = func(orderID string) (*domain.GatewayPayout, error) {
		if orderID != id.String() {
			t.Fatalf("reconciler must query by our order id, got %s", orderID)
		}
		return &domain.GatewayPayout{ProviderRef: "prov-1", Status: domain.PayoutPaid}, nil
	}

	rec := NewReconciler(pays, items, gw, fakeTx{}, ReconcilerConfig{StuckAfter: time.Minute}, nil)
	res, err := rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Resolved != 1 {
		t.Fatalf("expected 1 resolved, got %+v", res)
	}
	got, _ := pays.GetByID(ctx, id)
	if got.Status != domain.PayoutPaid {
		t.Fatalf("expected paid, got %s", got.Status)
	}
}

func TestReconcile_ResolvesStuckPayoutToFailedAndReleases(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	pays := newFakePayouts()
	items := newFakeItems()
	gw := &fakeGateway{}

	id := uuid.New()
	entry := uuid.New()
	_ = pays.Create(ctx, &domain.Payout{
		ID: id, RestaurantID: rid, AmountMinor: 5000, Currency: domain.CurrencyKZT,
		Status: domain.PayoutSent, Method: domain.PayoutMethodFreedomPayCardToken,
		IdempotencyKey: "payout:" + id.String(), StatusChangedAt: time.Now().Add(-time.Hour),
	})
	_ = items.CreateBatch(ctx, []domain.PayoutItem{{PayoutID: id, LedgerEntryID: entry, RestaurantID: rid, AmountSignedMinor: 5000, Currency: domain.CurrencyKZT}})

	gw.getFn = func(string) (*domain.GatewayPayout, error) {
		return &domain.GatewayPayout{Status: domain.PayoutFailed, FailureMessage: "insufficient balance"}, nil
	}
	rec := NewReconciler(pays, items, gw, fakeTx{}, ReconcilerConfig{StuckAfter: time.Minute}, nil)
	if _, err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := pays.GetByID(ctx, id)
	if got.Status != domain.PayoutFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	if _, stillClaimed := items.byEntry[entry]; stillClaimed {
		t.Fatal("a reconciled-to-failed payout must release its claim")
	}
}

func TestReconcile_UnknownBumpsAndKeepsSent(t *testing.T) {
	ctx := context.Background()
	pays := newFakePayouts()
	items := newFakeItems()
	gw := &fakeGateway{}
	id := uuid.New()
	_ = pays.Create(ctx, &domain.Payout{
		ID: id, RestaurantID: uuid.New(), AmountMinor: 5000, Currency: domain.CurrencyKZT,
		Status: domain.PayoutSent, Method: domain.PayoutMethodFreedomPayCardToken,
		IdempotencyKey: "payout:" + id.String(), StatusChangedAt: time.Now().Add(-time.Hour),
	})
	gw.getFn = func(string) (*domain.GatewayPayout, error) { return nil, domain.ErrProviderOutcomeUnknown }

	rec := NewReconciler(pays, items, gw, fakeTx{}, ReconcilerConfig{StuckAfter: time.Minute, MaxAttempts: 3}, nil)
	res, err := rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Bumped != 1 || res.Resolved != 0 {
		t.Fatalf("an unknown answer must bump, not resolve: %+v", res)
	}
	got, _ := pays.GetByID(ctx, id)
	if got.Status != domain.PayoutSent || got.ReconcileAttempts != 1 {
		t.Fatalf("expected still sent with 1 attempt, got %s / %d", got.Status, got.ReconcileAttempts)
	}
}
