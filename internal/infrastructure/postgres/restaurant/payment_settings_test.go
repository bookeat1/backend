package restaurant

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestGetPaymentOverride(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Payment Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A fresh venue has no override at all: every field is nil, meaning "use
	// the global PAYMENTS_* default".
	o, err := repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.PaymentsEnabled != nil || o.DepositRequired != nil || o.DepositAmountMinor != nil ||
		o.PreorderPaymentRequired != nil || o.ServiceFeeBps != nil || o.Provider != nil {
		t.Errorf("fresh venue override = %+v, want all nil", o)
	}

	// Set every column directly (no writer exists yet for these — admin-panel
	// writes are out of scope for this change) and confirm the reader sees them.
	_, err = pool.Exec(ctx, `UPDATE restaurants SET payments_enabled=true, deposit_required=true,
		deposit_amount_minor=150000, preorder_payment_required=true, service_fee_bps=500,
		payment_provider='tiptoppay' WHERE id=$1`, m.ID)
	if err != nil {
		t.Fatalf("seed override: %v", err)
	}

	o, err = repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after seed: %v", err)
	}
	if o.PaymentsEnabled == nil || !*o.PaymentsEnabled {
		t.Errorf("payments_enabled = %v, want true", o.PaymentsEnabled)
	}
	if o.DepositRequired == nil || !*o.DepositRequired {
		t.Errorf("deposit_required = %v, want true", o.DepositRequired)
	}
	if o.DepositAmountMinor == nil || *o.DepositAmountMinor != 150000 {
		t.Errorf("deposit_amount_minor = %v, want 150000", o.DepositAmountMinor)
	}
	if o.PreorderPaymentRequired == nil || !*o.PreorderPaymentRequired {
		t.Errorf("preorder_payment_required = %v, want true", o.PreorderPaymentRequired)
	}
	// A fresh venue has no pre-order minimum (migration 0042, nullable).
	if o.PreorderMinAmountMinor != nil {
		t.Errorf("preorder_min_amount_minor = %v, want nil on a fresh venue", *o.PreorderMinAmountMinor)
	}
	if o.ServiceFeeBps == nil || *o.ServiceFeeBps != 500 {
		t.Errorf("service_fee_bps = %v, want 500", o.ServiceFeeBps)
	}
	if o.Provider == nil || *o.Provider != domain.ProviderTipTopPay {
		t.Errorf("provider = %v, want tiptoppay", o.Provider)
	}

	// An unrecognised provider code (no FK/CHECK on this column) must not
	// resolve to a provider the registry cannot find — it falls back to the
	// global default, exactly like resolveSettings' other override fields.
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET payment_provider='not_a_real_provider' WHERE id=$1`, m.ID); err != nil {
		t.Fatalf("seed bogus provider: %v", err)
	}
	o, err = repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after bogus provider: %v", err)
	}
	if o.Provider != nil {
		t.Errorf("provider override = %v, want nil for an unrecognised code", *o.Provider)
	}

	if _, err := repo.GetPaymentOverride(ctx, uuid.New()); err != domain.ErrNotFound {
		t.Errorf("missing restaurant err = %v, want ErrNotFound", err)
	}
}

func TestUpdatePreorderSettings(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Preorder Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enable pre-order with a minimum; the reader must see both.
	min := int64(500000)
	yes, no := true, false
	if _, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{EnabledSet: true, Enabled: &yes, MinAmountSet: true, MinAmountMinor: &min}); err != nil {
		t.Fatalf("update preorder settings: %v", err)
	}
	o, err := repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.PreorderPaymentRequired == nil || !*o.PreorderPaymentRequired {
		t.Errorf("preorder_payment_required = %v, want true", o.PreorderPaymentRequired)
	}
	if o.PreorderMinAmountMinor == nil || *o.PreorderMinAmountMinor != 500000 {
		t.Errorf("preorder_min_amount_minor = %v, want 500000", o.PreorderMinAmountMinor)
	}

	// A patch that omits the minimum keeps it (switch-only write), and a patch
	// that omits the flag keeps the flag.
	if _, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{EnabledSet: true, Enabled: &no}); err != nil {
		t.Fatalf("update preorder settings (switch only): %v", err)
	}
	o, err = repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after switch-only: %v", err)
	}
	if o.PreorderPaymentRequired == nil || *o.PreorderPaymentRequired {
		t.Errorf("preorder_payment_required = %v, want false", o.PreorderPaymentRequired)
	}
	if o.PreorderMinAmountMinor == nil || *o.PreorderMinAmountMinor != 500000 {
		t.Errorf("switch-only write changed the minimum: %v, want 500000 kept", o.PreorderMinAmountMinor)
	}
	newMin := int64(100)
	if _, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{MinAmountSet: true, MinAmountMinor: &newMin}); err != nil {
		t.Fatalf("update preorder settings (min only): %v", err)
	}
	o, _ = repo.GetPaymentOverride(ctx, m.ID)
	if o.PreorderPaymentRequired == nil || *o.PreorderPaymentRequired || o.PreorderMinAmountMinor == nil || *o.PreorderMinAmountMinor != 100 {
		t.Errorf("min-only write: flag=%v min=%v, want false kept and 100", o.PreorderPaymentRequired, o.PreorderMinAmountMinor)
	}

	// Back to inherit: an explicit NULL flag reads back as nil, and clearing the
	// minimum reads back as nil.
	if _, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{EnabledSet: true, MinAmountSet: true}); err != nil {
		t.Fatalf("update preorder settings (clear): %v", err)
	}
	o, err = repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if o.PreorderPaymentRequired != nil {
		t.Errorf("preorder_payment_required = %v, want nil (inherit)", *o.PreorderPaymentRequired)
	}
	if o.PreorderMinAmountMinor != nil {
		t.Errorf("preorder_min_amount_minor = %v, want nil after clear", *o.PreorderMinAmountMinor)
	}

	// The DB CHECK rejects a negative floor (defense in depth behind the usecase
	// bound). Write it raw to prove the constraint, not the Go path.
	if _, err := pool.Exec(ctx, `UPDATE restaurants SET preorder_min_amount_minor=-1 WHERE id=$1`, m.ID); err == nil {
		t.Errorf("negative preorder_min_amount_minor was accepted, want CHECK violation")
	}

	if _, err := repo.UpdatePreorderSettings(ctx, uuid.New(), domain.PreorderSettingsPatch{EnabledSet: true, Enabled: &yes}); err != domain.ErrNotFound {
		t.Errorf("update on missing restaurant err = %v, want ErrNotFound", err)
	}
}

// The write returns the row's before/after captured by the same statement, and
// two concurrent writers each see a consistent, non-overlapping before/after
// chain (the audit line is built from it).
func TestUpdatePreorderSettingsReturnsBeforeAfter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()
	m := &domain.Restaurant{ID: uuid.New(), Name: "Audit Bistro", City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	yes, no := true, false
	min := int64(900)

	c, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{EnabledSet: true, Enabled: &yes, MinAmountSet: true, MinAmountMinor: &min})
	if err != nil {
		t.Fatal(err)
	}
	if c.OldEnabled != nil || c.NewEnabled == nil || !*c.NewEnabled || c.OldMinAmountMinor != nil || c.NewMinAmountMinor == nil || *c.NewMinAmountMinor != 900 {
		t.Fatalf("first change = %+v, want nil->true and nil->900", c)
	}
	// Omitted min: it stays 900 on both sides of the change.
	c, err = repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{EnabledSet: true, Enabled: &no})
	if err != nil {
		t.Fatal(err)
	}
	if c.OldEnabled == nil || !*c.OldEnabled || c.NewEnabled == nil || *c.NewEnabled ||
		c.OldMinAmountMinor == nil || *c.OldMinAmountMinor != 900 || c.NewMinAmountMinor == nil || *c.NewMinAmountMinor != 900 {
		t.Fatalf("second change = %+v, want true->false, min 900 both sides", c)
	}

	// Concurrent writers of the minimum: every "new" of one write must be the
	// "old" of exactly one other, i.e. the row lock serialises them.
	const n = 8
	changes := make([]domain.PreorderSettingsChange, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := int64(1000 + i)
			ch, err := repo.UpdatePreorderSettings(ctx, m.ID, domain.PreorderSettingsPatch{MinAmountSet: true, MinAmountMinor: &v})
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			changes[i] = ch
		}(i)
	}
	wg.Wait()
	olds := map[int64]int{}
	for _, ch := range changes {
		olds[*ch.OldMinAmountMinor]++
	}
	for _, ch := range changes {
		delete(olds, *ch.NewMinAmountMinor)
	}
	// After cancelling every "new" against an "old", only the initial 900 is left.
	if len(olds) != 1 || olds[900] != 1 {
		t.Fatalf("concurrent before/after chain is not serialised, unmatched olds = %v", olds)
	}
}

// TestPaymentMethodsRoundTrip: a fresh venue defaults to card on / kaspi off
// (migration 0116, existing behaviour), and UpdatePaymentMethods is read back.
func TestPaymentMethodsRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	m := &domain.Restaurant{ID: uuid.New(), Name: "Methods Bistro", City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	o, err := repo.GetPaymentOverride(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.KaspiEnabled == nil || *o.KaspiEnabled || o.CardEnabled == nil || !*o.CardEnabled {
		t.Fatalf("fresh venue methods = kaspi %v card %v, want false/true", o.KaspiEnabled, o.CardEnabled)
	}

	on := true
	if err := repo.UpdatePaymentMethods(ctx, m.ID, &on, true, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	o, _ = repo.GetPaymentOverride(ctx, m.ID)
	if o.PaymentsEnabled == nil || !*o.PaymentsEnabled || !*o.KaspiEnabled || *o.CardEnabled {
		t.Fatalf("after update: %+v", o)
	}
	if err := repo.UpdatePaymentMethods(ctx, m.ID, nil, false, true); err != nil {
		t.Fatalf("update nil: %v", err)
	}
	o, _ = repo.GetPaymentOverride(ctx, m.ID)
	if o.PaymentsEnabled != nil {
		t.Fatalf("payments_enabled = %v, want NULL (inherit)", *o.PaymentsEnabled)
	}
	if err := repo.UpdatePaymentMethods(ctx, uuid.New(), nil, true, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing venue: err = %v, want ErrNotFound", err)
	}
}
