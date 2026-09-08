// Package payouts is the restaurant-payout usecase: it tracks what BookEat owes
// each venue (computed from the payment ledger) and pays it out through an
// acquirer payout gateway (FreedomPay "выплаты"), with the same money-safety
// discipline as usecase/payments — DB-level CAS for every status change, an
// idempotency key so a retried send never double-pays, a claim table so a
// ledger entry is settled through at most one live payout, and a reconciler
// that resolves a payout stranded in `sent`.
//
// This package never imports internal/infrastructure/*: it depends only on the
// narrow domain ports below, wired to their Postgres/adapter implementations in
// bootstrap.
package payouts

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. Role is the GLOBAL role; the per-restaurant
// staff role is resolved by permissionChecker, exactly like usecase/admin.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant",
// per the domain RBAC matrix. Implemented by restaurants.ManagerUseCase. It is
// unaware of the global superadmin — the usecase checks RoleAdmin FIRST and
// bypasses this, the same contract every other HasPermission call site follows.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Ports groups the domain dependencies of the payout usecase, so bootstrap
// wires one struct instead of a long positional constructor.
type Ports struct {
	Perms        permissionChecker
	Destinations domain.PayoutDestinationRepository
	Payouts      domain.PayoutRepository
	Items        domain.PayoutItemRepository
	Owed         domain.OwedReader
	Gateway      domain.PayoutGateway // increment 1: FreedomPay only
	Tx           domain.TxManager
	// Ledger books the money and the fee of a CONFIRMED payout. Optional: when
	// it is nil the fee is still recorded on the payout row, only the
	// double-entry mirror is skipped (logged loudly, never silently).
	Ledger domain.PayoutLedgerRepository
	// Venues resolves each venue's IANA timezone for the venue-local day
	// boundary. Used only by the daily pass; nil elsewhere.
	Venues domain.PayoutVenueReader
	// Settings holds per-venue overrides of the platform payout policy.
	// Optional: when nil every venue simply gets the platform default, which is
	// exactly the behaviour before per-venue settings existed.
	Settings domain.PayoutSettingsRepository
}

// Config is the money policy of the payout usecase: what a payout costs and who
// pays for it. Both come from env (see bootstrap.PayoutsConfig) with the
// FreedomPay tariff of 14.07.2026 as defaults.
type Config struct {
	// FeeBps is the acquirer's payout rate in basis points. 190 = 1.9%.
	FeeBps int
	// FeeMinimumMinor is the per-payout floor in minor units. 30000 = 300 ₸.
	// This floor, not the rate, is what makes small payouts expensive and daily
	// batching worth doing.
	FeeMinimumMinor int64
	// FeeBearer is the WHO-PAYS policy. Defaults to the platform: a venue then
	// always receives exactly the amount its statement showed.
	FeeBearer domain.PayoutFeeBearer
	// PlatformPolicy is the payout threshold + max-hold cap that applies to a
	// venue with no settings of its own (env: PAYOUTS_MIN_AMOUNT_MINOR,
	// PAYOUTS_MAX_HOLD_DAYS).
	//
	// It lives on the usecase Config, not on DailyConfig, on purpose: the daily
	// runner ENFORCES the effective policy and the venue-facing read endpoint
	// REPORTS it, and those two numbers drifting apart would mean showing a
	// venue a rule it is not actually paid by. One struct, one source.
	PlatformPolicy domain.PayoutPolicy
}

const (
	// defaultPayoutFeeBps is FreedomPay's payout rate for a KZ bank card,
	// merchant questionnaire of 14.07.2026: 1.9%.
	defaultPayoutFeeBps = 190
	// defaultPayoutFeeMinimumMinor is the same tariff's per-payout floor:
	// 300 ₸ = 30000 tiyn.
	defaultPayoutFeeMinimumMinor int64 = 30000
	// defaultMinPayoutMinor — 10 000 ₸ (1 000 000 tiyn), the platform-wide
	// payout threshold (owner decision, unchanged by per-venue settings).
	//
	// The 300 ₸ floor makes the fee a percentage that falls as the payout
	// grows: 300 ₸ on 10 000 ₸ is 3.0%, on 5 000 ₸ it is 6%, on 1 000 ₸ it is
	// 30%. 3% is the worst case the owner already accepted when choosing daily
	// batching over per-booking payouts, so 10 000 ₸ is the threshold that
	// holds the cost at that accepted ceiling without holding a venue's money
	// longer than necessary.
	defaultMinPayoutMinor int64 = 1_000_000
	// defaultMaxHoldDays — 7 days (owner decision, 25.07.2026). A venue whose
	// turnover never reaches the threshold still gets paid weekly; the payout
	// pays the acquirer's floor, and that is the accepted price of not sitting
	// on someone else's money.
	defaultMaxHoldDays = 7
)

// withDefaults fills the FreedomPay tariff and the safe bearer policy.
//
// Known limit, stated rather than hidden: a zero-value field means "not
// configured", so a deployment that genuinely wants a ZERO payout fee cannot
// express it here — it would get the tariff defaults back. That is the
// deliberate direction of the safety: forgetting to configure the fee must
// over-state the cost, never under-state it. A real zero-fee tariff would be
// modelled as an explicit flag, not as a zero.
func (c Config) withDefaults() Config {
	if c.FeeBps <= 0 {
		c.FeeBps = defaultPayoutFeeBps
	}
	if c.FeeMinimumMinor <= 0 {
		c.FeeMinimumMinor = defaultPayoutFeeMinimumMinor
	}
	if !c.FeeBearer.Valid() {
		c.FeeBearer = domain.PayoutFeeBearerPlatform
	}
	if c.PlatformPolicy.MinPayoutMinor <= 0 {
		c.PlatformPolicy.MinPayoutMinor = defaultMinPayoutMinor
	}
	if c.PlatformPolicy.MaxHoldDays <= 0 {
		// Same direction of safety as the fee: an unconfigured hold cap must
		// mean "we do eventually pay", never "we may hold forever". A
		// deployment that genuinely wants unlimited holding expresses it
		// per-venue (max_hold_days = 0), which is an explicit, audited row.
		c.PlatformPolicy.MaxHoldDays = defaultMaxHoldDays
	}
	return c
}
