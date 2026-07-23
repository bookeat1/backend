// Package payments is the application logic that turns the payments domain
// (internal/domain/payment*.go) and the acquirer adapters
// (internal/infrastructure/payment/*) into the scenarios a guest and a
// restaurant actually need: pay for a booking, receive and apply a webhook,
// capture a hold on seating, release a hold on rejection, and settle a
// cancellation or a no-show.
//
// Layering notes, same convention as usecase/bookings:
//   - this package never imports internal/infrastructure/*; the two external
//     capabilities it needs from there (which gateway to use for a provider,
//     and the venue's payment policy) are declared as minimal local ports
//     below and bound to the concrete implementations in bootstrap/deps.go;
//   - Config mirrors bootstrap.PaymentsConfig so this layer stays free of any
//     bootstrap import (same shape as bookings.Config / auth.Config).
//
// Money rule, repeated because it is the one rule that must never regress
// here: every amount is computed by the SERVER from domain.Money /
// domain.ApplyBasisPoints, never trusted from a client, never a float
// (spec §3, §8).
package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. UserID is nil for a guest checkout
// without an account (domain.Payment.UserID mirrors this) — such a guest can
// still create and read "their" payment because the transport layer only
// reaches this package after verifying the booking's own contact OTP; there is
// no anonymous access to someone else's payment by guessing an id.
type Actor struct {
	UserID *uuid.UUID
	Role   domain.Role
}

// staff reports whether the actor acts on behalf of a venue or admin. This
// alone is NOT authorization for a money-moving action scoped to one
// restaurant — see authorizeStaffForRestaurant, which additionally checks
// which restaurant. Kept for the read-only / "is this a staff caller at all"
// checks where that is enough (see status.go doc comment).
func (a Actor) staff() bool { return a.Role == domain.RoleRestaurant || a.Role == domain.RoleAdmin }

// isUser reports whether uid is this actor's own user id.
func (a Actor) isUser(uid *uuid.UUID) bool {
	return a.UserID != nil && uid != nil && *a.UserID == *uid
}

// authorizeStaffForRestaurant is the venue-scoped staff gate used by every
// money-moving action (capture, void, settle): RoleAdmin may act on any
// restaurant; RoleRestaurant may act ONLY on a restaurant it manages.
//
// Report item #13: before this, `actor.staff()` alone was the whole check —
// it verifies the ROLE but never the TENANT, so any authenticated restaurant
// staff account could capture, void or settle a hold belonging to a DIFFERENT
// venue's booking just by knowing (or guessing) its booking id. This closes
// that hole the same way usecase/bookings.authorize does, via the same
// managerChecker port bound to restaurants.ManagerUseCase.
func authorizeStaffForRestaurant(ctx context.Context, managers managerChecker, actor Actor, restaurantID uuid.UUID) error {
	switch actor.Role {
	case domain.RoleAdmin:
		return nil
	case domain.RoleRestaurant:
		if actor.UserID == nil {
			return fmt.Errorf("%w: no authenticated staff actor", domain.ErrUnauthorized)
		}
		ok, err := managers.Manages(ctx, *actor.UserID, restaurantID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: not staff of this restaurant", domain.ErrForbidden)
		}
		return nil
	default:
		return fmt.Errorf("%w: only venue staff can do this", domain.ErrForbidden)
	}
}

// authorizeStaffPermission is authorizeStaffForRestaurant's stricter sibling:
// RoleAdmin still may act on any restaurant, but a RoleRestaurant actor must
// additionally hold perm at THIS restaurant per their domain.StaffRole (the
// RBAC matrix — see internal/domain/rbac.go), not merely be staff of it at
// all. Used by RefundUseCase.Settle (domain.PermPaymentRefund): the owner's
// spec is explicit that a hostess must never be able to settle a refund,
// even though a hostess passes the plain "is this staff of the restaurant"
// check that capture/void/create/status still use.
func authorizeStaffPermission(ctx context.Context, managers managerChecker, actor Actor, restaurantID uuid.UUID, perm domain.Permission) error {
	switch actor.Role {
	case domain.RoleAdmin:
		return nil
	case domain.RoleRestaurant:
		if actor.UserID == nil {
			return fmt.Errorf("%w: no authenticated staff actor", domain.ErrUnauthorized)
		}
		ok, err := managers.HasPermission(ctx, *actor.UserID, restaurantID, perm)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: this staff role cannot %s at this restaurant", domain.ErrForbidden, perm)
		}
		return nil
	default:
		return fmt.Errorf("%w: only venue staff can do this", domain.ErrForbidden)
	}
}

// gatewayResolver is the minimal slice of infrastructure/payment.Registry this
// package needs: pick the adapter for a NEW payment (Resolve, which falls back
// when the venue's preferred provider is disabled) and the adapter for an
// EXISTING payment regardless of the enabled flag (ForRefund — refunds,
// voids and webhooks for money already touched must keep working even after
// an acquirer is switched off, spec §9.1).
type gatewayResolver interface {
	Resolve(ctx context.Context, preferred domain.PaymentProvider) (domain.PaymentGateway, error)
	ForRefund(provider domain.PaymentProvider) (domain.PaymentGateway, error)
}

// bookingReader is the minimal slice of domain.BookingRepository this package
// needs: read a booking to compute what it owes and who is allowed to pay it.
type bookingReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Booking, error)
}

// bookingItemReader is the minimal slice of domain.BookingItemRepository this
// package needs: pre-ordered lines are priced at booking time (spec: "frozen
// at booking time"), so this is the only source of the pre-order base amount.
type bookingItemReader interface {
	ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingItem, error)
}

// restaurantPaymentSettings is the minimal slice of restaurant data this
// package needs: the venue's optional override of the global payment policy
// (restaurants.payments_enabled / deposit_* / preorder_payment_required /
// service_fee_bps / payment_provider — all NULLABLE, migration 0007).
//
// KNOWN GAP (disclosed, not silent): domain.Restaurant does not carry these
// columns as Go fields yet — only the domain.PaymentSettingsOverride shape
// exists, with no repository behind it. Whoever wires bootstrap/deps.go for
// this package must add a concrete adapter that reads the seven restaurants
// columns above into this shape; until then, GlobalOnlySettings (settings.go)
// is the only implementation, and every venue runs on the env defaults.
type restaurantPaymentSettings interface {
	GetPaymentOverride(ctx context.Context, restaurantID uuid.UUID) (domain.PaymentSettingsOverride, error)
}

// managerChecker answers whether a user manages a restaurant (Manages, the
// same port shape as usecase/bookings.managerChecker) and, additionally,
// whether they hold a specific domain.Permission there (HasPermission, RBAC
// foundation — see authorizeStaffPermission below). Both are bound to the
// same restaurants.ManagerUseCase in bootstrap/deps.go. See
// authorizeStaffForRestaurant (report item #13) for Manages' history.
type managerChecker interface {
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// cancelDeadlineResolver derives the real, server-computed cancel-before
// deadline for a booking, instead of trusting a caller-supplied value (report
// item #15): a booking's cancel deadline is `starts_at - policy.CancelDeadline`,
// where the policy is the venue's resolved booking policy — a computation
// that already lives in usecase/bookings and must not be duplicated (and
// possibly drift) here. Bound in bootstrap to a small adapter over that
// resolution.
type cancelDeadlineResolver interface {
	CancelDeadlineFor(ctx context.Context, booking domain.Booking) (time.Time, error)
}
