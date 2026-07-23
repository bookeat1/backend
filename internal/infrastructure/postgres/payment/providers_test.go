package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// payment_providers is seed data (migration 0007), never truncated by
// paymentTables/setup — these tests reset it to its known starting state on
// both sides so they can run in any order alongside the rest of the package
// without leaking is_enabled/is_default flags into another test.
func setupProviders(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	resetProviders(t, pool)
	t.Cleanup(func() { resetProviders(t, pool) })
	return pool, context.Background()
}

func resetProviders(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE payment_providers SET is_enabled=false, is_default=false,
		 priority = CASE provider WHEN 'freedompay' THEN 100 ELSE 200 END`); err != nil {
		t.Fatalf("reset payment_providers: %v", err)
	}
}

func TestProvidersListAndGetByCode(t *testing.T) {
	pool, ctx := setupProviders(t)
	repo := NewProviders(pool)

	all, err := repo.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %d rows, err=%v, want 2", len(all), err)
	}
	if all[0].Provider != domain.ProviderFreedomPay || all[1].Provider != domain.ProviderTipTopPay {
		t.Fatalf("list order by priority = %+v, want freedompay then tiptoppay", all)
	}

	got, err := repo.GetByCode(ctx, domain.ProviderFreedomPay)
	if err != nil || got.Provider != domain.ProviderFreedomPay {
		t.Fatalf("get by code: %+v, err=%v", got, err)
	}
	if _, err := repo.GetByCode(ctx, domain.PaymentProvider("unknown")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get by code(unknown) = %v, want ErrNotFound", err)
	}
}

func TestProvidersUpdateEnabledAndDefault(t *testing.T) {
	pool, ctx := setupProviders(t)
	repo := NewProviders(pool)

	fp, err := repo.GetByCode(ctx, domain.ProviderFreedomPay)
	if err != nil {
		t.Fatalf("get freedompay: %v", err)
	}
	fp.IsEnabled = true
	fp.IsDefault = true
	if err := repo.Update(ctx, fp); err != nil {
		t.Fatalf("update: %v", err)
	}

	enabled, err := repo.ListEnabled(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].Provider != domain.ProviderFreedomPay {
		t.Fatalf("list enabled: %+v, err=%v", enabled, err)
	}

	def, err := repo.GetDefault(ctx)
	if err != nil || def.Provider != domain.ProviderFreedomPay {
		t.Fatalf("get default: %+v, err=%v", def, err)
	}

	if err := repo.Update(ctx, &domain.PaymentProviderSetting{Provider: domain.PaymentProvider("unknown"), IsEnabled: true}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update(unknown provider) = %v, want ErrNotFound", err)
	}
}

// TestProvidersOnlyOneDefaultConflicts proves idx_payment_providers_default is
// translated into domain.ErrAlreadyExists — an admin flipping a second
// provider's is_default=true while another already carries it must be
// rejected, not silently leave two defaults.
func TestProvidersOnlyOneDefaultConflicts(t *testing.T) {
	pool, ctx := setupProviders(t)
	repo := NewProviders(pool)

	fp, err := repo.GetByCode(ctx, domain.ProviderFreedomPay)
	if err != nil {
		t.Fatalf("get freedompay: %v", err)
	}
	fp.IsDefault = true
	if err := repo.Update(ctx, fp); err != nil {
		t.Fatalf("set freedompay default: %v", err)
	}

	ttp, err := repo.GetByCode(ctx, domain.ProviderTipTopPay)
	if err != nil {
		t.Fatalf("get tiptoppay: %v", err)
	}
	ttp.IsDefault = true
	if err := repo.Update(ctx, ttp); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("set tiptoppay default too = %v, want ErrAlreadyExists", err)
	}

	// The original default must be unaffected by the rejected attempt.
	def, err := repo.GetDefault(ctx)
	if err != nil {
		// Not enabled yet — GetDefault requires is_enabled too; enable it to check.
		fp.IsEnabled = true
		if err := repo.Update(ctx, fp); err != nil {
			t.Fatalf("enable freedompay: %v", err)
		}
		def, err = repo.GetDefault(ctx)
	}
	if err != nil || def.Provider != domain.ProviderFreedomPay {
		t.Fatalf("get default after rejected conflict: %+v, err=%v", def, err)
	}
}
