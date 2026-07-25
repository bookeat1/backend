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
	Enabled            bool
	DefaultProvider    domain.PaymentProvider
	ServiceFeeBps      int
	RefundAcquiringBps int
	// RefundAcquiringBpsByProvider overrides RefundAcquiringBps for a specific
	// acquirer: what is kept when money travels back is the acquirer's rule,
	// not ours, and it differs between them. A provider that is absent from the
	// map uses RefundAcquiringBps. Read through refundAcquiringBpsFor, never
	// directly — a missing key and a stored 0 mean different things.
	RefundAcquiringBpsByProvider map[domain.PaymentProvider]int
	DepositDefaultMinor          int64
	DepositRequired              bool
	PreorderPaymentRequired      bool
	HoldTTL                      time.Duration
	// FreeCancelWindow is the global default free-cancellation window for the
	// money path, applied to any restaurant that has not overridden
	// free_cancel_window_minutes. Owner-confirmed default 120 minutes (see
	// withDefaults / migration 0034).
	FreeCancelWindow time.Duration
}

// Package-level fallbacks, applied to any zero-valued Config field — same
// pattern as bookings.Config.withDefaults.
const (
	defaultServiceFeeBps = 350 // 3.5%
	// Owner decision (2026-07-25): nothing is withheld from a guest's refund —
	// a timely cancellation returns the full charged amount and the acquirer's
	// cost is absorbed off the guest's side. Kept configurable for the day that
	// changes, but 0 is the default and an explicit 0 must survive withDefaults.
	defaultRefundAcquiringBps = 0
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
	// Only a NEGATIVE value is nonsense here: 0 is the intended production
	// setting ("withhold nothing"), so it must not be replaced by a fallback.
	if c.RefundAcquiringBps < 0 {
		c.RefundAcquiringBps = defaultRefundAcquiringBps
	}
	if c.HoldTTL <= 0 {
		c.HoldTTL = defaultHoldTTL
	}
	if c.FreeCancelWindow <= 0 {
		c.FreeCancelWindow = defaultFreeCancelWindow
	}
	// A per-provider rate that is negative is dropped rather than clamped: the
	// provider then falls back to the global rate, which is what an
	// unconfigured provider gets anyway. Filtered into a NEW map — Config is a
	// value, but a map inside it is shared, and withDefaults must not mutate
	// what the caller handed us.
	if len(c.RefundAcquiringBpsByProvider) > 0 {
		clean := make(map[domain.PaymentProvider]int, len(c.RefundAcquiringBpsByProvider))
		for p, bps := range c.RefundAcquiringBpsByProvider {
			if bps >= 0 {
				clean[p] = bps
			}
		}
		c.RefundAcquiringBpsByProvider = clean
	}
	return c
}

// refundAcquiringBpsFor is the rate withheld from a refund on THIS payment's
// acquirer: the provider's own entry when it has one, otherwise the global
// rate. Callers must never read RefundAcquiringBpsByProvider directly — a
// provider with an explicit 0 and a provider that was never configured look
// identical in the map otherwise.
func (c Config) refundAcquiringBpsFor(p domain.PaymentProvider) int {
	if bps, ok := c.RefundAcquiringBpsByProvider[p]; ok {
		return bps
	}
	return c.RefundAcquiringBps
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
