package domain

import (
	"context"
	"time"
)

// PaymentProvider is an acquirer code, stored as VARCHAR and used as the
// primary key of payment_providers. The domain knows the codes, never the
// protocols — those live in infrastructure/payment/<provider> (spec §2).
type PaymentProvider string

const (
	ProviderFreedomPay PaymentProvider = "freedompay"
	ProviderTipTopPay  PaymentProvider = "tiptoppay"
	// ProviderPartnersPay is a registered acquirer code with a template adapter
	// behind it (infrastructure/payment/partnerspay). It is seeded DISABLED in
	// the registry migration and its adapter's every method currently returns
	// "not implemented until the API contract is known" — see that package's
	// doc comment for what is still missing.
	ProviderPartnersPay PaymentProvider = "partnerspay"
)

// Valid reports whether p is a known provider code.
func (p PaymentProvider) Valid() bool {
	return p == ProviderFreedomPay || p == ProviderTipTopPay || p == ProviderPartnersPay
}

// PaymentProviderSetting is a row of the acquirer registry, managed from the
// admin panel. Credentials are never here — keys live in env only (spec §8).
//
// Disabling a provider blocks NEW payments through it and nothing else: refunds
// for money already taken keep going through that adapter (owner decision,
// spec §9.1). That is why usecases must resolve the gateway for a refund from
// the payment's own Provider field, not from this registry.
type PaymentProviderSetting struct {
	Provider  PaymentProvider
	IsEnabled bool
	IsDefault bool
	// Priority orders fallback candidates, lowest first.
	Priority  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PaymentProviderRepository reads and maintains the acquirer registry.
type PaymentProviderRepository interface {
	// List returns every registered provider ordered by priority.
	List(ctx context.Context) ([]PaymentProviderSetting, error)
	// ListEnabled returns only the enabled providers, ordered by priority.
	ListEnabled(ctx context.Context) ([]PaymentProviderSetting, error)
	// GetByCode returns one provider or ErrNotFound.
	GetByCode(ctx context.Context, provider PaymentProvider) (*PaymentProviderSetting, error)
	// GetDefault returns the enabled default provider, or ErrNotFound when no
	// provider is both enabled and marked as default. "No enabled acquirer" is
	// a legitimate state — it means the platform is not taking money right now
	// — and must be reported, not guessed around.
	GetDefault(ctx context.Context) (*PaymentProviderSetting, error)
	// Update writes the admin-editable flags. Only one row may carry
	// is_default (idx_payment_providers_default).
	Update(ctx context.Context, s *PaymentProviderSetting) error
}
