// Package admin is the restaurant admin-panel usecase: an RBAC-guarded
// orchestration layer over the existing restaurant/menu/booking/schedule
// building blocks. It owns NO new permission system — every action is gated by
// the shared domain RBAC matrix (domain.Permission), resolved per (actor,
// restaurant) exactly like usecase/restaurants.ManagerUseCase does, so an owner
// of venue A can never touch venue B (they simply hold no permission there).
//
// It reuses the existing usecases/facades rather than reimplementing them:
// menu item CRUD goes through menu.Facade (IDOR + validation already there),
// restaurant profile through restaurants.Facade (read-modify-write already
// there), and every booking status change delegates to the existing
// bookings.StatusUseCase transitions — never an ad-hoc status write.
package admin

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/restaurants"
)

// permissionChecker answers "may this user perform perm at this restaurant",
// per the domain RBAC matrix. Implemented by restaurants.ManagerUseCase. It is
// unaware of the global superadmin — the usecase checks RoleAdmin FIRST and
// bypasses this, the same contract every other call site of HasPermission
// follows.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// restaurantStore is the slice of restaurants.Facade this package needs:
// reading the venue's own profile and updating it (read-modify-write; only the
// fields we map are touched — see UpdateProfile).
type restaurantStore interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
	Update(ctx context.Context, id uuid.UUID, in restaurants.SaveInput) (*domain.RestaurantAggregate, error)
}

// menuStore is the slice of menu.Facade this package needs. Every mutating
// method takes restaurantID and enforces item ownership against it (IDOR guard
// lives in menu.Facade / the repo's restaurant_id predicate).
type menuStore interface {
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, lang *string) ([]domain.MenuItem, error)
	Categories(ctx context.Context) ([]domain.MenuCategory, error)
	Create(ctx context.Context, restaurantID uuid.UUID, in menu.ItemInput) (*domain.MenuItem, error)
	Update(ctx context.Context, restaurantID, itemID uuid.UUID, in menu.ItemInput) (*domain.MenuItem, error)
	Delete(ctx context.Context, restaurantID, itemID uuid.UUID) error
	SetAvailable(ctx context.Context, restaurantID, itemID uuid.UUID, available bool) error
	SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, itemIDs []uuid.UUID, available bool) (int, error)
}

// workingHoursStore is the slice of the restaurant related-collections repo
// this package needs: the venue's regular weekly hours.
type workingHoursStore interface {
	ListWorkingHours(ctx context.Context, restaurantID uuid.UUID) ([]domain.WorkingHours, error)
	ReplaceWorkingHours(ctx context.Context, restaurantID uuid.UUID, items []domain.WorkingHours) error
}

// guestStore reads the venue's aggregated guest list (read-only, derived from
// bookings).
type guestStore interface {
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantGuest, error)
}

// bookingLister is the venue booking calendar. Implemented by bookings.Facade,
// which pins the restaurant filter and authorizes staff access itself.
type bookingLister interface {
	ListByRestaurant(ctx context.Context, actor bookings.Actor, restaurantID uuid.UUID, f domain.BookingFilter) ([]domain.Booking, int, error)
}

// bookingTransitioner is the EXISTING booking state machine. Every admin-panel
// booking mutation delegates here so the transition table, audit history and
// outbox events stay the single source of truth — this package never writes a
// booking status directly. Implemented by bookings.StatusUseCase.
type bookingTransitioner interface {
	Confirm(ctx context.Context, actor bookings.Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
	Reject(ctx context.Context, actor bookings.Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
	Cancel(ctx context.Context, actor bookings.Actor, id uuid.UUID, in bookings.CancelInput) (*domain.Booking, error)
	NoShow(ctx context.Context, actor bookings.Actor, id uuid.UUID, reason *string) (*domain.Booking, error)
}
