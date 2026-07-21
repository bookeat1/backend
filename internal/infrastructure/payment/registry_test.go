package payment

import (
	"context"
	"errors"
	"testing"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// hand-written fakes (project convention: no mock framework)
// ---------------------------------------------------------------------------

type fakeGateway struct{ name domain.PaymentProvider }

func (f fakeGateway) Authorize(context.Context, domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	return nil, nil
}
func (f fakeGateway) Capture(context.Context, string, domain.Money) (*domain.GatewayPayment, error) {
	return nil, nil
}
func (f fakeGateway) Void(context.Context, string) error { return nil }
func (f fakeGateway) Refund(context.Context, string, domain.Money) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (f fakeGateway) Get(context.Context, string) (*domain.GatewayPayment, error) { return nil, nil }
func (f fakeGateway) VerifyWebhook([]byte, map[string]string) (*domain.WebhookEvent, error) {
	return nil, nil
}
func (f fakeGateway) Name() domain.PaymentProvider { return f.name }

type fakeSettings struct {
	rows map[domain.PaymentProvider]domain.PaymentProviderSetting
	err  error
}

func (f *fakeSettings) List(context.Context) ([]domain.PaymentProviderSetting, error) {
	return f.ordered(false), f.err
}

func (f *fakeSettings) ListEnabled(context.Context) ([]domain.PaymentProviderSetting, error) {
	return f.ordered(true), f.err
}

func (f *fakeSettings) ordered(onlyEnabled bool) []domain.PaymentProviderSetting {
	var out []domain.PaymentProviderSetting
	for _, s := range f.rows {
		if onlyEnabled && !s.IsEnabled {
			continue
		}
		out = append(out, s)
	}
	// stable order by priority, as the repository contract promises
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Priority < out[j-1].Priority; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (f *fakeSettings) GetByCode(_ context.Context, p domain.PaymentProvider) (*domain.PaymentProviderSetting, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.rows[p]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &s, nil
}

func (f *fakeSettings) GetDefault(context.Context) (*domain.PaymentProviderSetting, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, s := range f.rows {
		if s.IsDefault && s.IsEnabled {
			return &s, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeSettings) Update(context.Context, *domain.PaymentProviderSetting) error { return nil }

func settings(rows ...domain.PaymentProviderSetting) *fakeSettings {
	m := make(map[domain.PaymentProvider]domain.PaymentProviderSetting, len(rows))
	for _, r := range rows {
		m[r.Provider] = r
	}
	return &fakeSettings{rows: m}
}

func row(p domain.PaymentProvider, enabled, isDefault bool, priority int) domain.PaymentProviderSetting {
	return domain.PaymentProviderSetting{Provider: p, IsEnabled: enabled, IsDefault: isDefault, Priority: priority}
}

// ---------------------------------------------------------------------------

func TestNewRegistryRejectsDuplicateAndUnknownGateways(t *testing.T) {
	repo := settings()

	if _, err := NewRegistry(repo, "", fakeGateway{domain.ProviderFreedomPay}, fakeGateway{domain.ProviderFreedomPay}); err == nil {
		t.Error("duplicate gateway accepted")
	}
	if _, err := NewRegistry(repo, "", fakeGateway{"payme"}); !errors.Is(err, ErrProviderUnknown) {
		t.Errorf("unknown gateway: err = %v, want ErrProviderUnknown", err)
	}
	if _, err := NewRegistry(repo, "payme"); !errors.Is(err, ErrProviderUnknown) {
		t.Errorf("unknown fallback: err = %v, want ErrProviderUnknown", err)
	}
	if _, err := NewRegistry(nil, ""); err == nil {
		t.Error("nil settings repository accepted")
	}
}

func TestRegistryForRequiresConfiguredAndEnabled(t *testing.T) {
	repo := settings(
		row(domain.ProviderFreedomPay, true, true, 100),
		row(domain.ProviderTipTopPay, false, false, 200),
	)
	r, err := NewRegistry(repo, "", fakeGateway{domain.ProviderFreedomPay}, fakeGateway{domain.ProviderTipTopPay})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()

	t.Run("enabled and configured", func(t *testing.T) {
		g, err := r.For(ctx, domain.ProviderFreedomPay)
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		if g.Name() != domain.ProviderFreedomPay {
			t.Errorf("got %s", g.Name())
		}
	})

	t.Run("disabled is a domain error, not a panic", func(t *testing.T) {
		_, err := r.For(ctx, domain.ProviderTipTopPay)
		if !errors.Is(err, ErrProviderDisabled) {
			t.Fatalf("err = %v, want ErrProviderDisabled", err)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("err = %v, want it to wrap domain.ErrValidation", err)
		}
	})

	t.Run("unknown code", func(t *testing.T) {
		_, err := r.For(ctx, "payme")
		if !errors.Is(err, ErrProviderUnknown) {
			t.Fatalf("err = %v, want ErrProviderUnknown", err)
		}
	})

	t.Run("known but no adapter wired", func(t *testing.T) {
		bare, err := NewRegistry(repo, "", fakeGateway{domain.ProviderFreedomPay})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		_, err = bare.For(ctx, domain.ProviderTipTopPay)
		if !errors.Is(err, ErrProviderNotConfigured) {
			t.Fatalf("err = %v, want ErrProviderNotConfigured", err)
		}
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want it to wrap domain.ErrNotFound", err)
		}
	})
}

// Disabling an acquirer must not strand money already taken through it
// (spec §9.1).
func TestRegistryForRefundIgnoresTheEnabledFlag(t *testing.T) {
	repo := settings(row(domain.ProviderTipTopPay, false, false, 200))
	r, _ := NewRegistry(repo, "", fakeGateway{domain.ProviderTipTopPay})

	g, err := r.ForRefund(domain.ProviderTipTopPay)
	if err != nil {
		t.Fatalf("ForRefund: %v", err)
	}
	if g.Name() != domain.ProviderTipTopPay {
		t.Errorf("got %s", g.Name())
	}
	if _, err := r.ForRefund("payme"); !errors.Is(err, ErrProviderUnknown) {
		t.Errorf("err = %v, want ErrProviderUnknown", err)
	}
}

func TestRegistryResolve(t *testing.T) {
	tests := []struct {
		name      string
		rows      []domain.PaymentProviderSetting
		fallback  domain.PaymentProvider
		preferred domain.PaymentProvider
		want      domain.PaymentProvider
		wantErr   error
	}{
		{
			name:      "venue preference wins when enabled",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, true, 100), row(domain.ProviderTipTopPay, true, false, 200)},
			preferred: domain.ProviderTipTopPay,
			want:      domain.ProviderTipTopPay,
		},
		{
			name:      "disabled preference falls back to the default",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, true, 100), row(domain.ProviderTipTopPay, false, false, 200)},
			preferred: domain.ProviderTipTopPay,
			want:      domain.ProviderFreedomPay,
		},
		{
			name:      "unknown preference falls back to the default",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, true, 100)},
			preferred: "payme",
			want:      domain.ProviderFreedomPay,
		},
		{
			name:      "no preference uses the default",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, true, 100)},
			preferred: "",
			want:      domain.ProviderFreedomPay,
		},
		{
			name:      "no is_default row: env fallback, if enabled",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, false, 100), row(domain.ProviderTipTopPay, true, false, 50)},
			fallback:  domain.ProviderFreedomPay,
			preferred: "",
			want:      domain.ProviderFreedomPay,
		},
		{
			name:      "no default, no usable env fallback: lowest priority enabled",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, true, false, 100), row(domain.ProviderTipTopPay, true, false, 50)},
			preferred: "",
			want:      domain.ProviderTipTopPay,
		},
		{
			name:      "nothing enabled is reported, not guessed around",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderFreedomPay, false, false, 100), row(domain.ProviderTipTopPay, false, false, 200)},
			preferred: domain.ProviderFreedomPay,
			wantErr:   ErrNoEnabledProvider,
		},
		{
			name:      "default points at an acquirer we cannot speak to: another enabled one is used",
			rows:      []domain.PaymentProviderSetting{row(domain.ProviderTipTopPay, true, true, 200), row(domain.ProviderFreedomPay, true, false, 100)},
			preferred: "",
			want:      domain.ProviderFreedomPay,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateways := []domain.PaymentGateway{fakeGateway{domain.ProviderFreedomPay}}
			// The last case deliberately leaves tiptoppay unwired.
			if tc.name != "default points at an acquirer we cannot speak to: another enabled one is used" {
				gateways = append(gateways, fakeGateway{domain.ProviderTipTopPay})
			}
			r, err := NewRegistry(settings(tc.rows...), tc.fallback, gateways...)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}

			g, err := r.Resolve(context.Background(), tc.preferred)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if g.Name() != tc.want {
				t.Errorf("resolved %s, want %s", g.Name(), tc.want)
			}
		})
	}
}

// An infrastructure failure must never be silently downgraded to a fallback:
// "the database is down" is not "this venue's acquirer is off".
func TestRegistryResolvePropagatesRepositoryFailures(t *testing.T) {
	boom := errors.New("connection refused")
	repo := settings(row(domain.ProviderFreedomPay, true, true, 100))
	repo.err = boom

	r, _ := NewRegistry(repo, "", fakeGateway{domain.ProviderFreedomPay})
	if _, err := r.Resolve(context.Background(), domain.ProviderFreedomPay); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the repository error", err)
	}
}

func TestRegistryConfigured(t *testing.T) {
	r, _ := NewRegistry(settings(), "", fakeGateway{domain.ProviderFreedomPay})
	if !r.Configured(domain.ProviderFreedomPay) {
		t.Error("freedompay should be configured")
	}
	if r.Configured(domain.ProviderTipTopPay) {
		t.Error("tiptoppay should not be configured")
	}
}
