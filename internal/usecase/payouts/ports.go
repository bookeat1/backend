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
}

const (
	// defaultPayoutFeeBps is FreedomPay's payout rate for a KZ bank card,
	// merchant questionnaire of 14.07.2026: 1.9%.
	defaultPayoutFeeBps = 190
	// defaultPayoutFeeMinimumMinor is the same tariff's per-payout floor:
	// 300 ₸ = 30000 tiyn.
	defaultPayoutFeeMinimumMinor int64 = 30000
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
	return c
}
