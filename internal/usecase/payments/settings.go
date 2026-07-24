package payments

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Config is the global (level-1) payment policy, mirroring
// bootstrap.PaymentsConfig so this package stays free of a bootstrap import.
//
// Report item #10: DepositRequired and PreorderPaymentRequired used to exist
// ONLY as a restaurant override (domain.PaymentSettingsOverride), with no
// global fallback anywhere in Config. Since GlobalOnlySettings is the only
// restaurantPaymentSettings implementation wired so far (KNOWN GAP, see
// ports.go), every restaurant that never explicitly set these two override
// columns got Enabled=Enabled-from-env but DepositRequired=false and
// PreorderPaymentRequired=false unconditionally — resolveAmount then always
// hit "this booking requires no payment", i.e. payment creation was
// completely broken for every restaurant on the global defaults. These two
// fields close that: they are the same env-driven global default every other
// Config field already has.
type Config struct {
	Enabled                 bool
	DefaultProvider         domain.PaymentProvider
	ServiceFeeBps           int
	RefundAcquiringBps      int
	DepositDefaultMinor     int64
	DepositRequired         bool
	PreorderPaymentRequired bool
	HoldTTL                 time.Duration
	// FreeCancelWindow is the global default free-cancellation window for the
	// money path, applied to any restaurant that has not overridden
	// free_cancel_window_minutes. Owner-confirmed default 120 minutes (see
	// withDefaults / migration 0034).
	FreeCancelWindow time.Duration
}

// Package-level fallbacks, applied to any zero-valued Config field — same
// pattern as bookings.Config.withDefaults.
const (
	defaultServiceFeeBps      = 350               // 3.5%
	defaultRefundAcquiringBps = 100               // 1%
	defaultHoldTTL            = 96 * time.Hour    // stays below FreedomPay's 5-day auto-clear
	defaultFreeCancelWindow   = 120 * time.Minute // owner-confirmed default (migration 0034)
)

func (c Config) withDefaults() Config {
	if c.DefaultProvider == "" {
		c.DefaultProvider = domain.ProviderFreedomPay
	}
	if c.ServiceFeeBps <= 0 {
		c.ServiceFeeBps = defaultServiceFeeBps
	}
	if c.RefundAcquiringBps <= 0 {
		c.RefundAcquiringBps = defaultRefundAcquiringBps
	}
	if c.HoldTTL <= 0 {
		c.HoldTTL = defaultHoldTTL
	}
	if c.FreeCancelWindow <= 0 {
		c.FreeCancelWindow = defaultFreeCancelWindow
	}
	return c
}

// GlobalOnlySettings is a restaurantPaymentSettings that never has a venue
// override — every restaurant runs on the env defaults. It exists because no
// concrete adapter reads the restaurants.* payment columns yet (see the
// KNOWN GAP note on restaurantPaymentSettings in ports.go); bootstrap/deps.go
// can wire this in the meantime instead of blocking the whole feature on that
// missing column mapping.
type GlobalOnlySettings struct{}

// GetPaymentOverride always returns the zero value: no override.
func (GlobalOnlySettings) GetPaymentOverride(context.Context, uuid.UUID) (domain.PaymentSettingsOverride, error) {
	return domain.PaymentSettingsOverride{}, nil
}

// FreeCancelDeadlineFor is the money-path free-cancellation deadline for a
// booking: starts_at minus the restaurant's resolved free-cancel window
// (restaurants.free_cancel_window_minutes, else the global default). It is
// exported so bootstrap's cancelDeadlineResolver adapter derives the exact same
// value BOTH settlement flows (RefundUseCase.Settle and
// DepositCancellationUseCase) read, instead of each recomputing the window and
// risking drift — the same reason usecase/bookings.CancelDeadlineFor is
// exported.
func FreeCancelDeadlineFor(o domain.PaymentSettingsOverride, cfg Config, startsAt time.Time) time.Time {
	return startsAt.Add(-resolveSettings(o, cfg.withDefaults()).FreeCancelWindow)
}

// resolveSettings applies a venue's non-nil override fields on top of the
// global config — same resolution shape as bookings.resolvePolicy.
func resolveSettings(o domain.PaymentSettingsOverride, cfg Config) domain.PaymentSettings {
	s := domain.PaymentSettings{
		Enabled:                 cfg.Enabled,
		DepositAmountMinor:      cfg.DepositDefaultMinor,
		DepositRequired:         cfg.DepositRequired,
		PreorderPaymentRequired: cfg.PreorderPaymentRequired,
		ServiceFeeBps:           cfg.ServiceFeeBps,
		Provider:                cfg.DefaultProvider,
		FreeCancelWindow:        cfg.FreeCancelWindow,
	}
	// A venue override of the money-path free-cancellation window. Guard against
	// a negative stored value (the DB CHECK forbids it, but this layer must not
	// trust the column blindly, same defensive posture as the other overrides).
	if o.FreeCancelWindowMinutes != nil && *o.FreeCancelWindowMinutes >= 0 {
		s.FreeCancelWindow = time.Duration(*o.FreeCancelWindowMinutes) * time.Minute
	}
	if o.PaymentsEnabled != nil {
		s.Enabled = *o.PaymentsEnabled
	}
	if o.DepositRequired != nil {
		s.DepositRequired = *o.DepositRequired
	}
	if o.DepositAmountMinor != nil {
		s.DepositAmountMinor = *o.DepositAmountMinor
	}
	if o.PreorderPaymentRequired != nil {
		s.PreorderPaymentRequired = *o.PreorderPaymentRequired
	}
	if o.ServiceFeeBps != nil {
		s.ServiceFeeBps = *o.ServiceFeeBps
	}
	if o.Provider != nil {
		s.Provider = *o.Provider
	}
	return s
}
