package bookings

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller, taken from middleware.GetAuthUser by the
// transport layer. Every booking read and mutation resolves the actor's
// relation to the booking BEFORE touching data — there is no implicit allow.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// access is the resolved relation between an Actor and one booking.
type access struct {
	owner   bool // the booking belongs to this user
	manager bool // the actor manages the booking's restaurant
	admin   bool
}

// staff reports whether the actor acts on behalf of the venue.
func (a access) staff() bool { return a.manager || a.admin }

// actorType maps the relation to the audit-trail actor type. Staff wins over
// ownership: a manager cancelling their own booking is acting as the venue.
func (a access) actorType() domain.ActorType {
	switch {
	case a.admin:
		return domain.ActorAdmin
	case a.manager:
		return domain.ActorManager
	default:
		return domain.ActorGuest
	}
}

// senderType maps the relation to a chat sender type.
func (a access) senderType() domain.SenderType {
	if a.staff() {
		return domain.SenderRestaurant
	}
	return domain.SenderGuest
}

// authorize resolves the actor's relation to a booking. An unrelated guest gets
// ErrNotFound rather than ErrForbidden: 403 on someone else's booking id would
// confirm that the booking exists, which is an enumeration oracle. Staff of the
// wrong venue still get ErrForbidden — they are known, audited actors and the
// clearer error is worth more than hiding existence from them.
func authorize(ctx context.Context, managers managerChecker, actor Actor, b *domain.Booking) (access, error) {
	acc, err := resolveAccess(ctx, managers, actor, b.RestaurantID)
	if err != nil {
		return access{}, err
	}
	acc.owner = b.UserID != nil && *b.UserID == actor.UserID
	if !acc.owner && !acc.staff() {
		if actor.Role == domain.RoleRestaurant {
			return access{}, fmt.Errorf("%w: booking belongs to another restaurant", domain.ErrForbidden)
		}
		return access{}, fmt.Errorf("%w: booking", domain.ErrNotFound)
	}
	return acc, nil
}

// resolveAccess resolves the actor's relation to a restaurant, without any
// booking in hand (venue-scoped listings, creation). It does NOT reject
// unrelated users — the caller decides whether guest access is acceptable.
func resolveAccess(ctx context.Context, managers managerChecker, actor Actor, restaurantID uuid.UUID) (access, error) {
	if actor.UserID == uuid.Nil {
		return access{}, fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role == domain.RoleAdmin {
		return access{admin: true}, nil
	}
	if actor.Role != domain.RoleRestaurant {
		return access{}, nil
	}
	ok, err := managers.Manages(ctx, actor.UserID, restaurantID)
	if err != nil {
		return access{}, err
	}
	return access{manager: ok}, nil
}

// requireStaff resolves venue access and rejects anyone who is not a manager of
// that restaurant or an admin (spec §7: "manager of another venue → 403").
func requireStaff(ctx context.Context, managers managerChecker, actor Actor, restaurantID uuid.UUID) (access, error) {
	acc, err := resolveAccess(ctx, managers, actor, restaurantID)
	if err != nil {
		return access{}, err
	}
	if !acc.staff() {
		return access{}, fmt.Errorf("%w: not a manager of this restaurant", domain.ErrForbidden)
	}
	return acc, nil
}
