package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func ownerActor(uid uuid.UUID) Actor { return Actor{UserID: uid, Role: domain.RoleRestaurant} }
func adminActor() Actor              { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func guestActor(uid uuid.UUID) Actor { return Actor{UserID: uid, Role: domain.RoleUser} }

func TestManagerAssignChecksUserExists(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner}}}
	u := NewManagerUseCase(fm, &fakeUsers{err: domain.ErrNotFound}, &inlineTx{})
	_, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		UserID: uuid.New(), RestaurantID: rid, Role: domain.StaffRoleManager,
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("assign missing user err = %v, want ErrNotFound", err)
	}
}

func TestManagerAssignSuccess(t *testing.T) {
	rid, ownerID, uid := uuid.New(), uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner}}}
	fu := &fakeUsers{}
	u := NewManagerUseCase(fm, fu, &inlineTx{})

	m, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		RestaurantID: rid, UserID: uid, Role: domain.StaffRoleManager, WhatsappOptIn: true,
	})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if fm.created.RestaurantID != rid || fm.created.UserID != uid || fm.created.Role != domain.StaffRoleManager || !fm.created.WhatsappOptIn {
		t.Errorf("created = %+v, want RestaurantID=%v UserID=%v Role=manager WhatsappOptIn=true", fm.created, rid, uid)
	}
	// The new staff member's global role must be bumped from guest to
	// restaurant staff, so middleware.Auth's staff gate recognises them.
	if fu.updated == nil || fu.updated.Role != domain.RoleRestaurant {
		t.Errorf("assignee's global role = %+v, want RoleRestaurant", fu.updated)
	}
}

// TestManagerAssignDoesNotDowngradeAdmin: promoteToStaff must never touch an
// existing admin/staff account.
func TestManagerAssignDoesNotDowngradeAdmin(t *testing.T) {
	rid, ownerID, uid := uuid.New(), uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner}}}
	fu := &fakeUsers{user: &domain.User{ID: uid, Role: domain.RoleAdmin}}
	u := NewManagerUseCase(fm, fu, &inlineTx{})

	if _, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		RestaurantID: rid, UserID: uid, Role: domain.StaffRoleHostess,
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if fu.updated != nil {
		t.Errorf("must not touch an existing admin's global role, got update %+v", fu.updated)
	}
}

func TestManagerManages(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{{RestaurantID: rid, UserID: uid, Role: domain.StaffRoleManager}}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	ok, err := u.Manages(context.Background(), uid, rid)
	if err != nil || !ok {
		t.Errorf("Manages = %v, %v; want true, nil", ok, err)
	}
	ok, _ = u.Manages(context.Background(), uuid.New(), uuid.New())
	if ok {
		t.Error("Manages = true for unrelated restaurant, want false")
	}
}

// TestHasPermissionMatrix exercises HasPermission end-to-end (usecase +
// domain matrix together) for every staff role, matching the owner's spec.
func TestHasPermissionMatrix(t *testing.T) {
	rid := uuid.New()
	cases := []struct {
		role domain.StaffRole
		perm domain.Permission
		want bool
	}{
		{domain.StaffRoleHostess, domain.PermBookingManage, true},
		{domain.StaffRoleHostess, domain.PermPaymentCapture, true},
		{domain.StaffRoleHostess, domain.PermPaymentRefund, false},
		{domain.StaffRoleHostess, domain.PermStaffManage, false},
		{domain.StaffRoleManager, domain.PermPaymentRefund, true},
		{domain.StaffRoleManager, domain.PermStaffManage, false},
		{domain.StaffRoleOwner, domain.PermStaffManage, true},
		{domain.StaffRoleOwner, domain.PermPaymentRefund, true},
	}
	for _, c := range cases {
		uid := uuid.New()
		fm := &fakeManagers{rows: []domain.RestaurantManager{{RestaurantID: rid, UserID: uid, Role: c.role}}}
		u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
		got, err := u.HasPermission(context.Background(), uid, rid, c.perm)
		if err != nil {
			t.Fatalf("HasPermission(%s, %s): %v", c.role, c.perm, err)
		}
		if got != c.want {
			t.Errorf("HasPermission(%s, %s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestHasPermissionFalseForNonStaff(t *testing.T) {
	fm := &fakeManagers{}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	ok, err := u.HasPermission(context.Background(), uuid.New(), uuid.New(), domain.PermBookingManage)
	if err != nil || ok {
		t.Errorf("HasPermission for a non-staff user = %v, %v; want false, nil", ok, err)
	}
}

// --- staff.manage authorization on List/Assign/SetRole/Remove ---

func TestOwnerCanOnlyManageOwnRestaurant(t *testing.T) {
	ownRid, otherRid, ownerID := uuid.New(), uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: ownRid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	actor := ownerActor(ownerID)

	if _, err := u.List(context.Background(), actor, ownRid); err != nil {
		t.Errorf("List own restaurant: %v", err)
	}
	if _, err := u.List(context.Background(), actor, otherRid); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("List other restaurant err = %v, want ErrForbidden", err)
	}

	if _, err := u.Assign(context.Background(), actor, AssignManagerInput{
		RestaurantID: otherRid, UserID: uuid.New(), Role: domain.StaffRoleHostess,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("Assign to other restaurant err = %v, want ErrForbidden", err)
	}
}

// TestOwnerCreatesHostessOnlyInOwnRestaurant is the exact scenario named in
// the spec: an owner assigning a hostess to a DIFFERENT restaurant is
// rejected, but assigning one to their own restaurant succeeds.
func TestOwnerCreatesHostessOnlyInOwnRestaurant(t *testing.T) {
	ownRid, otherRid, ownerID, newHostess := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: ownRid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	actor := ownerActor(ownerID)

	if _, err := u.Assign(context.Background(), actor, AssignManagerInput{
		RestaurantID: otherRid, UserID: newHostess, Role: domain.StaffRoleHostess,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("assign hostess to a DIFFERENT restaurant err = %v, want ErrForbidden", err)
	}
	if _, err := u.Assign(context.Background(), actor, AssignManagerInput{
		RestaurantID: ownRid, UserID: newHostess, Role: domain.StaffRoleHostess,
	}); err != nil {
		t.Fatalf("assign hostess to OWN restaurant: %v", err)
	}
}

// TestCannotGrantRoleAtOrAboveOwn: "нельзя повысить роль выше своей" — an
// owner may create a manager or a hostess, never another owner.
func TestCannotGrantRoleAtOrAboveOwn(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	actor := ownerActor(ownerID)

	if _, err := u.Assign(context.Background(), actor, AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleOwner,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("owner granting owner err = %v, want ErrForbidden", err)
	}
	if _, err := u.Assign(context.Background(), actor, AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleManager,
	}); err != nil {
		t.Errorf("owner granting manager: %v", err)
	}
}

// TestSuperadminCanGrantOwner: a superadmin is unrestricted, unlike an owner.
func TestSuperadminCanGrantOwner(t *testing.T) {
	rid := uuid.New()
	fm := &fakeManagers{}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.Assign(context.Background(), adminActor(), AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleOwner,
	}); err != nil {
		t.Errorf("admin granting owner: %v", err)
	}
}

func TestAssignRejectsUnknownRole(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRole("waiter"),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("assign unknown role err = %v, want ErrValidation", err)
	}
}

func TestGuestCannotManageStaff(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	fm := &fakeManagers{}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.Assign(context.Background(), guestActor(uid), AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleHostess,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("guest assign err = %v, want ErrForbidden", err)
	}
}

// --- SetRole / Remove: target resolved by id, authorized against ITS
// restaurant (the cross-tenant IDOR the old admin-only gate used to prevent
// by refusing everyone but admins in the first place). ---

func TestSetRoleCrossTenantIsRejected(t *testing.T) {
	ownRid, otherRid, ownerID := uuid.New(), uuid.New(), uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: ownRid, UserID: ownerID, Role: domain.StaffRoleOwner},
		{ID: targetID, RestaurantID: otherRid, UserID: uuid.New(), Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.SetRole(context.Background(), ownerActor(ownerID), targetID, domain.StaffRoleManager); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("cross-tenant SetRole err = %v, want ErrForbidden", err)
	}
}

func TestSetRoleSuccess(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
		{ID: targetID, RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	m, err := u.SetRole(context.Background(), ownerActor(ownerID), targetID, domain.StaffRoleManager)
	if err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if m.Role != domain.StaffRoleManager {
		t.Errorf("role = %s, want manager", m.Role)
	}
}

func TestSetRoleCannotPromoteToOwner(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
		{ID: targetID, RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleManager},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.SetRole(context.Background(), ownerActor(ownerID), targetID, domain.StaffRoleOwner); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("SetRole to owner err = %v, want ErrForbidden", err)
	}
}

func TestRemoveCrossTenantIsRejected(t *testing.T) {
	ownRid, otherRid, ownerID := uuid.New(), uuid.New(), uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: ownRid, UserID: ownerID, Role: domain.StaffRoleOwner},
		{ID: targetID, RestaurantID: otherRid, UserID: uuid.New(), Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if err := u.Remove(context.Background(), ownerActor(ownerID), targetID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("cross-tenant Remove err = %v, want ErrForbidden", err)
	}
	// Confirm nothing was actually deleted.
	if _, err := fm.GetByID(context.Background(), targetID); err != nil {
		t.Errorf("target row was deleted despite ErrForbidden: %v", err)
	}
}

func TestRemoveSuccess(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
		{ID: targetID, RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if err := u.Remove(context.Background(), ownerActor(ownerID), targetID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := fm.GetByID(context.Background(), targetID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("target row still present after Remove: %v", err)
	}
}

func TestRemoveMissingIsNotFound(t *testing.T) {
	u := NewManagerUseCase(&fakeManagers{}, &fakeUsers{}, &inlineTx{})
	if err := u.Remove(context.Background(), adminActor(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Remove missing row err = %v, want ErrNotFound", err)
	}
}

// TestSuperadminBypassesTenantScoping: an admin manages ANY restaurant's
// staff without needing a restaurant_managers row of their own.
func TestSuperadminBypassesTenantScoping(t *testing.T) {
	rid := uuid.New()
	targetID := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: targetID, RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.List(context.Background(), adminActor(), rid); err != nil {
		t.Errorf("admin List: %v", err)
	}
	if _, err := u.SetRole(context.Background(), adminActor(), targetID, domain.StaffRoleManager); err != nil {
		t.Errorf("admin SetRole: %v", err)
	}
}
