package domain

import "testing"

// TestStaffPermissionMatrix is THE table test for the RBAC matrix given by
// the owner: each staff role can do exactly what it should, and nothing
// more. Any change to staffPermissions must update this table deliberately.
func TestStaffPermissionMatrix(t *testing.T) {
	allPerms := []Permission{PermBookingManage, PermPaymentCapture, PermPaymentRefund, PermStaffManage}

	want := map[StaffRole]map[Permission]bool{
		StaffRoleHostess: {
			PermBookingManage:  true,
			PermPaymentCapture: true,
			PermPaymentRefund:  false,
			PermStaffManage:    false,
		},
		StaffRoleManager: {
			PermBookingManage:  true,
			PermPaymentCapture: true,
			PermPaymentRefund:  true,
			PermStaffManage:    false,
		},
		StaffRoleOwner: {
			PermBookingManage:  true,
			PermPaymentCapture: true,
			PermPaymentRefund:  true,
			PermStaffManage:    true,
		},
	}

	for role, perms := range want {
		for _, perm := range allPerms {
			got := role.HasPermission(perm)
			if got != perms[perm] {
				t.Errorf("%s.HasPermission(%s) = %v, want %v", role, perm, got, perms[perm])
			}
		}
	}
}

// TestHostessCannotRefund is the single hard rule called out explicitly by
// the owner: a hostess must never be able to settle/refund a payment.
func TestHostessCannotRefund(t *testing.T) {
	if StaffRoleHostess.HasPermission(PermPaymentRefund) {
		t.Fatal("hostess must not have payment.refund")
	}
}

// TestUnknownRoleHasNoPermissions guards against a typo'd/empty role
// silently falling through to "no restriction".
func TestUnknownRoleHasNoPermissions(t *testing.T) {
	var unknown StaffRole = "waiter"
	for _, perm := range []Permission{PermBookingManage, PermPaymentCapture, PermPaymentRefund, PermStaffManage} {
		if unknown.HasPermission(perm) {
			t.Errorf("unknown role must not have %s", perm)
		}
	}
	if StaffRole("").HasPermission(PermBookingManage) {
		t.Error("empty role must not have any permission")
	}
}

func TestStaffRoleValid(t *testing.T) {
	for _, r := range []StaffRole{StaffRoleOwner, StaffRoleManager, StaffRoleHostess} {
		if !r.Valid() {
			t.Errorf("%s.Valid() = false, want true", r)
		}
	}
	if StaffRole("waiter").Valid() {
		t.Error(`"waiter".Valid() = true, want false`)
	}
	if StaffRole("").Valid() {
		t.Error(`"".Valid() = true, want false`)
	}
}

func TestStaffRoleOutranks(t *testing.T) {
	cases := []struct {
		a, b StaffRole
		want bool
	}{
		{StaffRoleOwner, StaffRoleManager, true},
		{StaffRoleOwner, StaffRoleHostess, true},
		{StaffRoleOwner, StaffRoleOwner, false}, // strict: cannot grant your OWN rank either
		{StaffRoleManager, StaffRoleHostess, true},
		{StaffRoleManager, StaffRoleOwner, false},
		{StaffRoleHostess, StaffRoleManager, false},
		{StaffRole("waiter"), StaffRoleHostess, false}, // unknown role outranks nothing
	}
	for _, c := range cases {
		if got := c.a.Outranks(c.b); got != c.want {
			t.Errorf("%s.Outranks(%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
