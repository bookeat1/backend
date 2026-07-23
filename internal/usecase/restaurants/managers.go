package restaurants

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for every staff-management action below.
// A global superadmin (Role == domain.RoleAdmin) bypasses all restaurant
// scoping; anyone else is authorized entirely by HasPermission against the
// TARGET restaurant — there is no separate "is this an owner" flag to trust,
// which is also how "only for your OWN restaurant" falls out for free (spec:
// "владелец — только для своего заведения").
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// ManagerUseCase manages a restaurant's staff roster (owner/manager/hostess
// rows in restaurant_managers) and answers the two questions the rest of the
// backend needs about it: "is this user staff of this restaurant at all"
// (Manages, role-agnostic — used by bookings/payments for the "any staff of
// the venue" checks) and "may this user do THIS specific thing at this
// restaurant" (HasPermission, backed by the domain.StaffRole matrix).
type ManagerUseCase interface {
	List(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.RestaurantManager, error)
	Assign(ctx context.Context, actor Actor, in AssignManagerInput) (*domain.RestaurantManager, error)
	SetRole(ctx context.Context, actor Actor, id uuid.UUID, role domain.StaffRole) (*domain.RestaurantManager, error)
	Remove(ctx context.Context, actor Actor, id uuid.UUID) error
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// userRepo is the minimal slice of the users repository this package needs:
// verifying a staff assignee exists (GetByID) and promoting a brand-new
// staff member's global role from guest to restaurant staff (Update — see
// promoteToStaff). Bound to the concrete user repo in bootstrap/deps.go.
type userRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	Update(ctx context.Context, u *domain.User) error
}

type managerUseCase struct {
	managers domain.RestaurantManagerRepository
	users    userRepo
	tx       domain.TxManager
}

// NewManagerUseCase constructs the ManagerUseCase.
func NewManagerUseCase(managers domain.RestaurantManagerRepository, users userRepo, tx domain.TxManager) ManagerUseCase {
	return &managerUseCase{managers: managers, users: users, tx: tx}
}

// AssignManagerInput assigns a user as staff of a restaurant with a specific
// StaffRole.
type AssignManagerInput struct {
	RestaurantID  uuid.UUID
	UserID        uuid.UUID
	Role          domain.StaffRole
	CreatedBy     *uuid.UUID
	WhatsappOptIn bool
	WhatsappPhone *string
}

// Manages reports whether userID is staff of restaurantID at ANY role — the
// role-agnostic check used by usecase/bookings and usecase/payments for
// "is this a staff caller of this specific venue at all" (capture, void,
// create-on-behalf, status reads, and every booking transition, none of
// which the owner's spec distinguishes by staff role).
func (u *managerUseCase) Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	ms, err := u.managers.ListByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.RestaurantID == restaurantID {
			return true, nil
		}
	}
	return false, nil
}

// staffRoleOf resolves userID's StaffRole at restaurantID. ok is false when
// userID is not staff of that restaurant at all.
func (u *managerUseCase) staffRoleOf(ctx context.Context, userID, restaurantID uuid.UUID) (role domain.StaffRole, ok bool, err error) {
	ms, err := u.managers.ListByUser(ctx, userID)
	if err != nil {
		return "", false, err
	}
	for _, m := range ms {
		if m.RestaurantID == restaurantID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

// HasPermission reports whether userID may perform perm at restaurantID,
// per their StaffRole there (domain.StaffRole.HasPermission). It does NOT
// know about superadmin — every call site checks actor.Role == RoleAdmin
// FIRST and bypasses this entirely (see authorizeStaffManage below, and
// usecase/payments.authorizeStaffPermission for the payments-side call site).
func (u *managerUseCase) HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	role, ok, err := u.staffRoleOf(ctx, userID, restaurantID)
	if err != nil || !ok {
		return false, err
	}
	return role.HasPermission(perm), nil
}

// authorizeStaffManage is the shared gate for List/Assign/SetRole/Remove: a
// superadmin always passes; anyone else must hold domain.PermStaffManage AT
// restaurantID — which only that restaurant's own owner has, per the RBAC
// matrix. This is also where "only for your OWN restaurant" comes from: the
// permission lookup is keyed by (actor, restaurantID), so an owner of
// restaurant A simply has no row — and therefore no permission — at
// restaurant B.
func (u *managerUseCase) authorizeStaffManage(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := u.HasPermission(ctx, actor.UserID, restaurantID, domain.PermStaffManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: only this restaurant's owner or a superadmin may manage its staff", domain.ErrForbidden)
	}
	return nil
}

// List returns restaurantID's full staff roster. Same authorization as
// Assign/SetRole/Remove — an owner may only list their OWN restaurant.
func (u *managerUseCase) List(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.RestaurantManager, error) {
	if err := u.authorizeStaffManage(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	return u.managers.ListByRestaurant(ctx, restaurantID)
}

// Assign creates a new staff row. Rules enforced here, all from the owner's
// spec:
//   - only the target restaurant's own owner, or a superadmin, may call this
//     (authorizeStaffManage);
//   - a non-admin caller (i.e. an owner) may never grant a role at or above
//     their own — in practice this means an owner may create a manager or a
//     hostess, never another owner (spec: "нельзя повысить роль выше своей");
//   - a superadmin is unrestricted and may grant any role, including owner
//     (bootstrapping a brand-new venue's first owner has to start somewhere).
func (u *managerUseCase) Assign(ctx context.Context, actor Actor, in AssignManagerInput) (*domain.RestaurantManager, error) {
	if err := u.authorizeStaffManage(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	if !in.Role.Valid() {
		return nil, fmt.Errorf("%w: unknown staff role %q", domain.ErrValidation, in.Role)
	}
	if actor.Role != domain.RoleAdmin {
		actorRole, _, err := u.staffRoleOf(ctx, actor.UserID, in.RestaurantID)
		if err != nil {
			return nil, err
		}
		if !actorRole.Outranks(in.Role) {
			return nil, fmt.Errorf("%w: cannot grant a role at or above your own", domain.ErrForbidden)
		}
	}
	if _, err := u.users.GetByID(ctx, in.UserID); err != nil {
		return nil, err // ErrNotFound when the assignee doesn't exist
	}
	m := &domain.RestaurantManager{
		RestaurantID: in.RestaurantID, UserID: in.UserID, Role: in.Role, CreatedBy: in.CreatedBy,
		WhatsappOptIn: in.WhatsappOptIn, WhatsappPhone: in.WhatsappPhone,
	}
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.managers.Create(ctx, m); err != nil {
			return err
		}
		return u.promoteToStaff(ctx, in.UserID)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// SetRole changes an existing staff member's role, under the same
// authorization and rank rules as Assign. The target row is resolved by id
// FIRST so the authorization check runs against the row's OWN restaurant,
// never a caller-supplied one — closing the same cross-tenant IDOR class
// Remove closes below.
func (u *managerUseCase) SetRole(ctx context.Context, actor Actor, id uuid.UUID, role domain.StaffRole) (*domain.RestaurantManager, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("%w: unknown staff role %q", domain.ErrValidation, role)
	}
	m, err := u.managers.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := u.authorizeStaffManage(ctx, actor, m.RestaurantID); err != nil {
		return nil, err
	}
	if actor.Role != domain.RoleAdmin {
		actorRole, _, err := u.staffRoleOf(ctx, actor.UserID, m.RestaurantID)
		if err != nil {
			return nil, err
		}
		if !actorRole.Outranks(role) {
			return nil, fmt.Errorf("%w: cannot grant a role at or above your own", domain.ErrForbidden)
		}
	}
	if err := u.managers.UpdateRole(ctx, id, role); err != nil {
		return nil, err
	}
	m.Role = role
	return m, nil
}

// Remove deletes a staff row. The target is resolved by id FIRST (same
// reasoning as SetRole): removeManager used to be admin-only specifically
// because deleting by managerID alone, scoped only by "is the caller a
// manager of the restaurant in the URL", would let a manager of venue A
// delete a staff row belonging to venue B just by knowing its id. Resolving
// the row first and authorizing against ITS restaurant closes that hole, so
// this can now safely be opened up to a restaurant's own owner too.
func (u *managerUseCase) Remove(ctx context.Context, actor Actor, id uuid.UUID) error {
	m, err := u.managers.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := u.authorizeStaffManage(ctx, actor, m.RestaurantID); err != nil {
		return err
	}
	return u.managers.Delete(ctx, id)
}

// promoteToStaff bumps a brand-new staff member's global User.Role from
// RoleUser to RoleRestaurant so middleware.Auth and middleware.RequireRole
// recognise them as venue staff. It never touches an existing RoleAdmin or
// RoleRestaurant account. This is a DB column, not a JWT claim — there is no
// token to reissue: middleware.Auth reloads the user fresh from the database
// on every single request (see its doc comment), so the very next request
// this user makes already carries the new role, guest OTP login included
// (auth/otp.go only sets Role on a BRAND NEW user row; an existing account's
// Role is never reset on login).
func (u *managerUseCase) promoteToStaff(ctx context.Context, userID uuid.UUID) error {
	user, err := u.users.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.Role == domain.RoleUser {
		user.Role = domain.RoleRestaurant
		return u.users.Update(ctx, user)
	}
	return nil
}
