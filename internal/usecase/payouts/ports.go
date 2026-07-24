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
}
