package restaurant

import (
	"context"
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
