package payment

import (
	"context"
	"errors"
	"fmt"

	"backend-core/internal/domain"
)

// Registry errors. All of them wrap a domain sentinel, so transport keeps
// mapping them (422 / 404) without learning what an acquirer is. None of them
// panics: an unknown or disabled provider is an ordinary, expected outcome of
// an admin toggling a switch, not a programming error.
var (
	// ErrProviderUnknown — the code is not a provider this build knows.
	ErrProviderUnknown = fmt.Errorf("unknown payment provider: %w", domain.ErrValidation)
	// ErrProviderNotConfigured — a known code with no adapter wired in (its
	// credentials are missing from env, so bootstrap skipped it).
	ErrProviderNotConfigured = fmt.Errorf("payment provider is not configured: %w", domain.ErrNotFound)
	// ErrProviderDisabled — the adapter exists but the registry row is off.
	// New payments are refused; refunds on money already taken are not (see
	// ForRefund).
	ErrProviderDisabled = fmt.Errorf("payment provider is disabled: %w", domain.ErrValidation)
	// ErrNoEnabledProvider — nothing is enabled. A legitimate state: the
	// platform is simply not taking money right now. Reported, never guessed
	// around.
	ErrNoEnabledProvider = fmt.Errorf("no enabled payment provider: %w", domain.ErrNotFound)
)

// Registry picks the gateway for a payment (spec §2, Registry/Strategy).
//
// It combines two sources of truth and keeps them apart on purpose:
//
//   - which adapters this process actually has (compiled in and holding valid
//     credentials from env) — the map below;
//   - which providers the business currently allows — the payment_providers
//     table, editable from the admin panel.
//
// A provider is usable for a NEW payment only when both agree. Switching
// acquirer is then a setting, not a release.
type Registry struct {
	gateways map[domain.PaymentProvider]domain.PaymentGateway
	settings domain.PaymentProviderRepository
	// fallback is PAYMENTS_DEFAULT_PROVIDER: the last resort when the registry
	// table marks no default. It still has to be enabled to be used.
	fallback domain.PaymentProvider
}

// NewRegistry wires the adapters this process has. Passing two adapters that
// report the same Name() is a wiring bug and is rejected here rather than
// silently letting one win.
func NewRegistry(settings domain.PaymentProviderRepository, fallback domain.PaymentProvider, gateways ...domain.PaymentGateway) (*Registry, error) {
	if settings == nil {
		return nil, errors.New("payment registry: settings repository is required")
	}
	if fallback != "" && !fallback.Valid() {
		return nil, fmt.Errorf("payment registry: fallback %q: %w", fallback, ErrProviderUnknown)
	}
	m := make(map[domain.PaymentProvider]domain.PaymentGateway, len(gateways))
	for _, g := range gateways {
		if g == nil {
			return nil, errors.New("payment registry: nil gateway")
		}
		name := g.Name()
		if !name.Valid() {
			return nil, fmt.Errorf("payment registry: gateway reports %q: %w", name, ErrProviderUnknown)
		}
		if _, dup := m[name]; dup {
			return nil, fmt.Errorf("payment registry: duplicate gateway for %q", name)
		}
		m[name] = g
	}
	return &Registry{gateways: m, settings: settings, fallback: fallback}, nil
}

// Configured reports whether an adapter for p exists in this process.
func (r *Registry) Configured(p domain.PaymentProvider) bool {
	_, ok := r.gateways[p]
	return ok
}

// ForRefund returns the adapter for a provider REGARDLESS of the enabled flag.
//
// Turning an acquirer off must not strand money that already went through it:
// refunds and reconciliation for existing payments keep using its adapter
// (spec §9.1). Callers resolve it from the payment's own Provider field, never
// from the registry table.
func (r *Registry) ForRefund(p domain.PaymentProvider) (domain.PaymentGateway, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("%q: %w", p, ErrProviderUnknown)
	}
	g, ok := r.gateways[p]
	if !ok {
		return nil, fmt.Errorf("%q: %w", p, ErrProviderNotConfigured)
	}
	return g, nil
}

// For returns the adapter to use for a NEW payment through p. It requires the
// adapter to exist AND the payment_providers row to be enabled.
func (r *Registry) For(ctx context.Context, p domain.PaymentProvider) (domain.PaymentGateway, error) {
	g, err := r.ForRefund(p)
	if err != nil {
		return nil, err
	}
	setting, err := r.settings.GetByCode(ctx, p)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%q: %w", p, ErrProviderNotConfigured)
		}
		return nil, fmt.Errorf("read provider %q: %w", p, err)
	}
	if !setting.IsEnabled {
		return nil, fmt.Errorf("%q: %w", p, ErrProviderDisabled)
	}
	return g, nil
}

// Resolve picks the gateway for a new payment given a venue's preference.
//
// Order, and the reason for it (spec §9.1): the venue's own choice wins when it
// is usable; otherwise we fall back rather than refuse, because a venue whose
// preferred acquirer was switched off must keep taking bookings. Falling back
// is logged by the caller as a configuration mismatch, not swallowed.
func (r *Registry) Resolve(ctx context.Context, preferred domain.PaymentProvider) (domain.PaymentGateway, error) {
	if preferred != "" {
		g, err := r.For(ctx, preferred)
		if err == nil {
			return g, nil
		}
		// Only a "this one is unusable" answer justifies a fallback. An
		// infrastructure failure must surface.
		if !errors.Is(err, ErrProviderDisabled) &&
			!errors.Is(err, ErrProviderNotConfigured) &&
			!errors.Is(err, ErrProviderUnknown) {
			return nil, err
		}
	}
	return r.Default(ctx)
}

// Default returns the gateway for the enabled default provider: the
// payment_providers row marked is_default, then PAYMENTS_DEFAULT_PROVIDER, then
// the enabled provider with the lowest priority.
func (r *Registry) Default(ctx context.Context) (domain.PaymentGateway, error) {
	if setting, err := r.settings.GetDefault(ctx); err == nil {
		if g, ok := r.gateways[setting.Provider]; ok {
			return g, nil
		}
		// The admin marked an acquirer we cannot speak to. Do not stop here:
		// another enabled provider below may still be able to take the money.
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("read default provider: %w", err)
	}

	enabled, err := r.settings.ListEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled providers: %w", err)
	}
	if r.fallback != "" {
		for _, s := range enabled {
			if s.Provider == r.fallback {
				if g, ok := r.gateways[r.fallback]; ok {
					return g, nil
				}
			}
		}
	}
	for _, s := range enabled { // already ordered by priority
		if g, ok := r.gateways[s.Provider]; ok {
			return g, nil
		}
	}
	return nil, ErrNoEnabledProvider
}

// MerchantIDFinder is an OPTIONAL capability: looking a payment up by OUR id
// instead of the acquirer's.
//
// It is not part of domain.PaymentGateway because not every acquirer supports
// it, and the domain must not carry a method half its adapters cannot honour.
// The reconciliation worker needs it for one specific hole: with a hosted
// payment page the acquirer-side transaction id only exists after the guest
// pays, so a payment whose "paid" webhook was lost has no id to call Get with —
// only ours (spec §5, §7).
//
// Both current adapters implement it (TipTopPay /v2/payments/find by InvoiceId,
// FreedomPay /g2g/status_v2 by pg_order_id). Callers must type-assert.
type MerchantIDFinder interface {
	FindByMerchantPaymentID(ctx context.Context, merchantPaymentID string) (*domain.GatewayPayment, error)
}
